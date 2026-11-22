// Package server 是 capy2api 的 HTTP 层：OpenAI / Anthropic 兼容端点 + 管理面板。
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/haohaomin/capy2api/internal/devicedaemon"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/config"
	"github.com/haohaomin/capy2api/internal/panel"
	"github.com/haohaomin/capy2api/internal/store"
)

// Version 由构建时注入。
var Version = "dev"

// Server 聚合配置、账号池与上游客户端。
// tenant 是一次请求的调用方身份（主密钥 = 管理员，其余为订阅 key）。
type tenant struct {
	ID     string
	Name   string
	Master bool
	Key    store.APIKey
}

type Server struct {
	cfgPtr atomic.Pointer[config.Config]
	store  *store.Store
	keys   *store.KeyStore
	log    *store.Log
	client *capy.Client
	sem    chan struct{}
	start  time.Time

	sessMu sync.Mutex
	sess   map[string]*capy.Session

	keySemMu  sync.Mutex
	keySem    map[string]chan struct{}
	pool      threadPool
	daemonMu  sync.RWMutex
	daemonPtr *devicedaemon.Daemon
	login     loginState
	createSem chan struct{}
	queueMu   sync.Mutex
	queueTill time.Time
	ctxMu     sync.Mutex
	lastCtx   map[string]any
	convos    *convRegistry
	modelMu   sync.Mutex
	models    map[string]modelsCache
	inflight  map[string]chan struct{}
}

type modelsCache struct {
	at     time.Time
	models []capy.Model
}

// New 构造服务。
func New(cfg *config.Config, st *store.Store, keys *store.KeyStore, lg *store.Log) *Server {
	conc := cfg.Capy.MaxConcurrent
	if conc <= 0 {
		conc = 2
	}
	srv := &Server{
		createSem: make(chan struct{}, 2),
		store:     st,
		keys:      keys,
		log:       lg,
		client:    capy.NewClient(cfg.Capy.APIBase, cfg.Capy.ClerkBase, cfg.Capy.ClerkJSVersion, cfg.Capy.Features, cfg.Capy.Surface, cfg.Capy.Timezone, cfg.Capy.ClientID),
		sem:       make(chan struct{}, conc),
		start:     time.Now(),
		keySem:    map[string]chan struct{}{},
		convos:    newConvRegistry(time.Duration(cfg.Capy.ConversationTTLSec)*time.Second, 256, filepath.Join(cfg.AuthDir, "conversations.json")),
		sess:      map[string]*capy.Session{},
		models:    map[string]modelsCache{},
		inflight:  map[string]chan struct{}{},
	}
	srv.cfgPtr.Store(cfg)
	return srv
}

// cfg 返回当前配置快照（指针不可变：写入方总是 Store 一个新对象）。
func (s *Server) cfg() *config.Config { return s.cfgPtr.Load() }

// Handler 组装全部路由。
// SetDaemon 挂上伪造设备（可选）。
func (s *Server) SetDaemon(d *devicedaemon.Daemon) {
	s.daemonMu.Lock()
	s.daemonPtr = d
	s.daemonMu.Unlock()
}

// daemon 返回当前伪造设备（可能为 nil）。
func (s *Server) daemon() *devicedaemon.Daemon {
	s.daemonMu.RLock()
	defer s.daemonMu.RUnlock()
	return s.daemonPtr
}

// ensureDaemon 用账号自己的设备凭据起一条伪造设备连接（没有凭据就跳过）。
func (s *Server) ensureDaemon(acc store.Account) {
	if s.daemon() != nil || acc.DeviceToken == "" || acc.DeviceID == "" {
		return
	}
	d := devicedaemon.Start(context.Background(), devicedaemon.Config{
		Token: acc.DeviceToken, DeviceID: acc.DeviceID,
		Workspace: s.cfg().Capy.DeviceWorkspace, Policy: s.cfg().Capy.DevicePolicy, BashAllowAll: s.cfg().Capy.BashAllowAll,
	})
	s.SetDaemon(d)
	log.Printf("伪造设备已启动：%s（workspace %s）", acc.DeviceID, s.cfg().Capy.DeviceWorkspace)
}

// HasDaemon 是否已经有伪造设备在跑。
func (s *Server) HasDaemon() bool { return s.daemon() != nil }

// EnsureDaemonFromStore 启动时给第一个有设备凭据的账号拉起伪造设备。
func (s *Server) EnsureDaemonFromStore() {
	for _, v := range s.store.List() {
		if acc, ok := s.store.Raw(v.ID); ok {
			s.ensureDaemon(acc)
		}
	}
}

// recordContext 记录最近一次回合的上下文构成（面板/接口透出用）。
func (s *Server) recordContext(model string, res *capy.TurnResult) {
	snap := map[string]any{
		"at": time.Now().Format(time.RFC3339), "model": model,
		"jam_id": res.JamID, "context_tokens": res.ContextTokens,
		"tools_used": res.ToolsUsed, "ttft_ms": res.TTFTMS,
		"tokens_per_second": res.TokPerSec, "transport": res.Transport,
	}
	s.ctxMu.Lock()
	s.lastCtx = snap
	s.ctxMu.Unlock()
}

// queueBlocked 上游排队期间直接快速失败，避免每个请求都白等 30 秒。
func (s *Server) queueBlocked(w http.ResponseWriter) bool {
	s.queueMu.Lock()
	till := s.queueTill
	s.queueMu.Unlock()
	if time.Now().Before(till) {
		w.Header().Set("retry-after", fmt.Sprintf("%d", int(time.Until(till).Seconds())+1))
		writeAPIError(w, http.StatusTooManyRequests, "上游正在排队，请稍后重试", "upstream_queued")
		return true
	}
	return false
}

func (s *Server) markQueue(active bool) {
	s.queueMu.Lock()
	if active {
		s.queueTill = time.Now().Add(30 * time.Second)
	} else {
		s.queueTill = time.Time{}
	}
	s.queueMu.Unlock()
}

// handleContext 透出上游注入的上下文构成：system/tools/mcp/skills/rules、本轮用过的工具、记忆拦截记录。
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	s.ctxMu.Lock()
	last := s.lastCtx
	s.ctxMu.Unlock()

	// Drive 各作用域的文件数：用户级 AGENTS.md 会被当作系统指令注入，所有线程共享。
	scopes := make([]map[string]any, 0, 4)
	if acc, err := s.store.Next(); err == nil {
		sess := s.sessionFor(acc)
		var vols []struct {
			ID        string  `json:"id"`
			Namespace string  `json:"namespace"`
			Name      string  `json:"name"`
			UserID    *string `json:"userId"`
			ProjectID *string `json:"projectId"`
			FileCount int     `json:"fileCount"`
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		derr := sess.Do(ctx, "GET", "/app/orgs/"+acc.OrgID+"/drive", acc.OrgID, nil, &vols)
		cancel()
		if derr != nil {
			log.Printf("WARN 读取 Drive 作用域失败: %v", derr)
		} else {
			for _, v := range vols {
				if v.FileCount == 0 {
					continue
				}
				scope := "org"
				if v.UserID != nil {
					scope = "user"
				}
				if v.ProjectID != nil {
					scope = "project"
				}
				scopes = append(scopes, map[string]any{
					"scope": scope, "volume_id": v.ID, "namespace": v.Namespace,
					"name": v.Name, "files": v.FileCount,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"policy":       s.cfg().Capy.DevicePolicy,
		"daemon":       s.daemonStatus(),
		"drive_scopes": scopes,
		"last_turn":    last,
		"note":         "drive_scopes 里 user 作用域的文件（如 AGENTS.md）会作为系统指令注入每一条线程；memory_attempts 是云端通过设备 read 尝试读取的记忆文件（已被策略拦截）。",
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	mux.HandleFunc("GET /v1/models/{id}", s.auth(s.handleModel))
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChat))
	mux.HandleFunc("POST /v1/messages", s.auth(s.handleAnthropic))
	mux.HandleFunc("POST /messages", s.auth(s.handleAnthropic))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.auth(s.handleCountTokens))
	mux.HandleFunc("POST /messages/count_tokens", s.auth(s.handleCountTokens))
	mux.HandleFunc("GET /v1/responses", s.auth(s.handleResponsesWSProbe))
	mux.HandleFunc("GET /status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /v1/capacity", s.auth(s.handleCapacity))
	mux.HandleFunc("POST /v2/chat/busy", s.auth(s.handleChatBusy))
	mux.HandleFunc("POST /v1/responses", s.auth(s.handleResponses))
	mux.HandleFunc("POST /v1/embeddings", s.auth(s.handleUnsupported))
	mux.HandleFunc("GET /v1/usage", s.auth(s.handleUsage))
	mux.HandleFunc("GET /v1/context", s.auth(s.handleContext))
	mux.HandleFunc("GET /capy/account", s.auth(s.handleUsage))

	mux.HandleFunc("GET /panel/api/overview", s.admin(s.handleOverview))
	mux.HandleFunc("POST /panel/api/login", s.handleLogin)
	mux.HandleFunc("POST /panel/api/logout", s.handleLogout)
	mux.HandleFunc("GET /panel/api/accounts", s.admin(s.handleAccounts))
	mux.HandleFunc("POST /panel/api/accounts", s.admin(s.handleAccountCreate))
	mux.HandleFunc("POST /panel/api/accounts/import", s.admin(s.handleAccountImport))
	mux.HandleFunc("POST /panel/api/accounts/browser-login", s.admin(s.handleBrowserLogin))
	mux.HandleFunc("GET /panel/api/accounts/browser-login/status", s.admin(s.handleBrowserLoginStatus))
	mux.HandleFunc("DELETE /panel/api/accounts/browser-login", s.admin(s.handleBrowserLoginCancel))
	mux.HandleFunc("PATCH /panel/api/accounts/{id}", s.admin(s.handleAccountPatch))
	mux.HandleFunc("DELETE /panel/api/accounts/{id}", s.admin(s.handleAccountDelete))
	mux.HandleFunc("POST /panel/api/accounts/{id}/probe", s.admin(s.handleAccountProbe))
	mux.HandleFunc("GET /panel/api/models", s.admin(s.handlePanelModels))
	mux.HandleFunc("GET /panel/api/config", s.admin(s.handleConfigGet))
	mux.HandleFunc("POST /panel/api/config", s.admin(s.handleConfigSet))
	mux.HandleFunc("GET /panel/api/logs", s.admin(s.handleLogs))
	mux.HandleFunc("GET /panel/api/stats", s.admin(s.handleStats))
	mux.HandleFunc("GET /panel/api/context", s.admin(s.handleContext))
	mux.HandleFunc("GET /panel/api/conversations", s.admin(s.handleConversations))
	mux.HandleFunc("POST /panel/api/conversations/cleanup", s.admin(s.handleConversationsCleanup))
	mux.HandleFunc("GET /panel/api/keys", s.admin(s.handleKeys))
	mux.HandleFunc("POST /panel/api/keys", s.admin(s.handleKeyCreate))
	mux.HandleFunc("PATCH /panel/api/keys/{id}", s.admin(s.handleKeyPatch))
	mux.HandleFunc("DELETE /panel/api/keys/{id}", s.admin(s.handleKeyDelete))
	mux.HandleFunc("GET /panel/api/import-status", s.admin(s.handleImportStatus))

	mux.HandleFunc("GET /panel/", s.handlePanel)
	mux.HandleFunc("GET /panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusFound)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/panel/", http.StatusFound)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "未知路径 " + r.URL.Path, "type": "not_found"},
		})
	})

	return withRecover(withRequestID(withPanelOrigin(withCORS(withLogging(mux)))))
}

// ------------------------------------------------------------------ middleware

type ctxKey string

const ctxRequestID ctxKey = "request_id"

// withRequestID 透传/生成请求 id：响应头 + 日志都用它，方便和上游排障对齐。
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("x-request-id"))
		if id == "" || len(id) > 64 {
			id = "req_" + shortID()
		}
		w.Header().Set("x-request-id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// withRecover 兜住 handler panic：返回 500 并留下日志，不让连接被裸断。
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, rec)
				writeAPIError(w, http.StatusInternalServerError, "网关内部错误", "internal_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withPanelOrigin 挡住跨站的面板写操作（cookie 是 SameSite=Lax，但显式校验更稳）。
func withPanelOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/panel/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := strings.TrimSpace(r.Header.Get("origin")); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"message": "跨站请求被拒绝"}})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-headers", "authorization,content-type,x-api-key,anthropic-version,anthropic-beta,openai-beta")
		w.Header().Set("access-control-allow-methods", "GET,POST,PATCH,DELETE,OPTIONS")
		w.Header().Set("access-control-max-age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/panel/api") || r.URL.Path == "/healthz" {
			return
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("%s %s -> %d (%dms) rid=%s", r.Method, r.URL.Path, status, time.Since(start).Milliseconds(), requestID(r))
	})
}

// ctxTenant 保存调用方身份。
const ctxTenant ctxKey = "tenant"

// auth 校验密钥并解析调用方身份（主密钥 / 订阅 key），同时做限速与配额检查。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, status, msg, code := s.resolveTenant(r)
		if t == nil {
			if status == http.StatusTooManyRequests {
				w.Header().Set("retry-after", "60")
			}
			if strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.HasPrefix(r.URL.Path, "/messages") {
				writeAnthropicError(w, status, msg)
			} else {
				writeAPIError(w, status, msg, code)
			}
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxTenant, t)))
	}
}

func tenantOf(r *http.Request) *tenant {
	if v, ok := r.Context().Value(ctxTenant).(*tenant); ok {
		return v
	}
	return &tenant{ID: "anonymous", Name: "anonymous", Master: true}
}

// resolveTenant 解析身份：主密钥 → 管理员；其余查订阅 key 表并校验配额/速率。
func (s *Server) resolveTenant(r *http.Request) (*tenant, int, string, string) {
	key := bearerKey(r)
	if key == "" || s.cfg().APIKey == "" {
		return nil, http.StatusUnauthorized, "无效的 API key", "invalid_api_key"
	}
	if subtleCompare(key, s.cfg().APIKey) {
		return &tenant{ID: "master", Name: "master", Master: true}, 0, "", ""
	}
	k, ok := s.keys.Lookup(key)
	if !ok {
		return nil, http.StatusUnauthorized, "无效、已禁用或已过期的 API key", "invalid_api_key"
	}
	if msg := k.QuotaExceeded(); msg != "" {
		return nil, http.StatusPaymentRequired, msg, "quota_exceeded"
	}
	if !s.keys.AllowRPM(k) {
		return nil, http.StatusTooManyRequests, "该 API key 超出每分钟请求上限", "rate_limited"
	}
	return &tenant{ID: k.ID, Name: k.Name, Key: k}, 0, "", ""
}

// accountFor 选择本次请求使用的 Capy 账号。
//
// 订阅 key 绑了账号就用绑定的那个（多租户隔离：Drive/记忆/线程都跟着账号走）；
// 没绑（或主密钥）走共享池轮换。
func (s *Server) accountFor(t *tenant) (store.Account, error) {
	if t != nil && !t.Master && t.Key.AccountID != "" {
		if acc, ok := s.store.Raw(t.Key.AccountID); ok && acc.Enabled {
			return acc, nil
		}
	}
	return s.store.Next()
}

// acquireKeySlot 限制单个订阅 key 的并发（MaxConcurrent=0 时用全局上限）。
func (s *Server) acquireKeySlot(ctx context.Context, t *tenant) (func(), error) {
	if t == nil || t.Master || t.Key.MaxConcurrent <= 0 {
		return func() {}, nil
	}
	s.keySemMu.Lock()
	sem := s.keySem[t.ID]
	if sem == nil {
		sem = make(chan struct{}, t.Key.MaxConcurrent)
		s.keySem[t.ID] = sem
	}
	s.keySemMu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func bearerKey(r *http.Request) string {
	if h := r.Header.Get("authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		if v := strings.TrimSpace(h[7:]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func (s *Server) keyOK(r *http.Request) bool {
	provided := ""
	if h := r.Header.Get("authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		provided = strings.TrimSpace(h[7:])
	}
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if provided == "" {
		if c, err := r.Cookie(sessionCookie); err == nil {
			provided = c.Value
		}
	}
	if provided == "" || s.cfg().APIKey == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg().APIKey)) == 1
}

const sessionCookie = "capy2api_admin"

// admin 校验面板会话（cookie 签名）或 Bearer api_key。
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.keyOK(r) {
			next(w, r)
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil && s.verifySession(c.Value) {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "请先登录面板"}})
	}
}

func (s *Server) signSession(exp int64) string {
	mac := hmac.New(sha256.New, []byte(s.cfg().APIKey))
	msg := strconv.FormatInt(exp, 10)
	mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifySession(v string) bool {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	expect := s.signSession(exp)
	return hmac.Equal([]byte(expect), []byte(v))
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, message, typ string) {
	if typ == "" {
		typ = "upstream_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": typ, "code": typ}})
}

// sessionFor 返回账号对应的上游会话。
func (s *Server) sessionFor(acc store.Account) *capy.Session {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if sess, ok := s.sess[acc.ID]; ok {
		if sess.Cookie != acc.Cookie || sess.SessionID != acc.SessionID {
			sess.Cookie = acc.Cookie
			sess.SessionID = acc.SessionID
			sess.Invalidate()
		}
		return sess
	}
	sess := &capy.Session{Client: s.client, Cookie: acc.Cookie, SessionID: acc.SessionID}
	s.sess[acc.ID] = sess
	return sess
}

// catalog 返回账号可用的免费模型（5 分钟缓存 + 单飞 + 失败回退旧目录）。
//
// 并发请求同时缺缓存时只放一个去拉上游，其余等结果；拉失败但手里有旧目录时
// 继续用旧的（过期总比 502 好），并记一条 WARN。
func (s *Server) catalog(ctx context.Context, acc store.Account, force bool) ([]capy.Model, error) {
	s.modelMu.Lock()
	entry, ok := s.models[acc.ID]
	if ok && !force && time.Since(entry.at) < 5*time.Minute && len(entry.models) > 0 {
		s.modelMu.Unlock()
		return entry.models, nil
	}
	if ch, running := s.inflight[acc.ID]; running {
		s.modelMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.modelMu.Lock()
		entry, ok = s.models[acc.ID]
		s.modelMu.Unlock()
		if ok && len(entry.models) > 0 {
			return entry.models, nil
		}
		return nil, fmt.Errorf("模型目录正在刷新，请稍后重试")
	}
	ch := make(chan struct{})
	s.inflight[acc.ID] = ch
	s.modelMu.Unlock()
	defer func() {
		s.modelMu.Lock()
		delete(s.inflight, acc.ID)
		s.modelMu.Unlock()
		close(ch)
	}()

	sess := s.sessionFor(acc)
	models, err := sess.Models(ctx, acc.OrgID)
	if err != nil {
		if ok && len(entry.models) > 0 {
			log.Printf("WARN 模型目录刷新失败，沿用旧目录（%s）: %v", acc.ID, err)
			return entry.models, nil
		}
		return nil, err
	}
	kept := make([]capy.Model, 0, len(models))
	for _, m := range models {
		// 显式写进 allowed_models 的模型放行（可用于有额度时选非广告的强模型），
		// 否则只保留免费广告模型。
		explicit := containsFold(s.cfg().Capy.AllowedModels, m.ID)
		if !m.FirstParty() && !explicit {
			continue
		}
		if len(s.cfg().Capy.AllowedModels) > 0 && !explicit {
			continue
		}
		kept = append(kept, m)
	}
	s.modelMu.Lock()
	s.models[acc.ID] = modelsCache{at: time.Now(), models: kept}
	s.modelMu.Unlock()
	return kept, nil
}

func (s *Server) invalidateModels(accID string) {
	s.modelMu.Lock()
	delete(s.models, accID)
	s.modelMu.Unlock()
}

// resolveModel 把客户端给的模型名映射到目录里的真实 id。
func resolveModel(want string, catalog []capy.Model) (capy.Model, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return capy.Model{}, errors.New("model 不能为空")
	}
	for _, m := range catalog {
		if m.ID == want {
			return m, nil
		}
	}
	for _, m := range catalog {
		if strings.EqualFold(m.ID, want) {
			return m, nil
		}
		if i := strings.LastIndex(m.ID, "/"); i >= 0 && strings.EqualFold(m.ID[i+1:], want) {
			return m, nil
		}
		if m.Name != "" && strings.EqualFold(m.Name, want) {
			return m, nil
		}
	}
	ids := make([]string, 0, len(catalog))
	for _, m := range catalog {
		ids = append(ids, m.ID)
	}
	if len(ids) == 0 {
		return capy.Model{}, errors.New("当前账号没有可用的免费广告模型")
	}
	// 通用客户端（Claude Code / Codex）会按自己的习惯填 claude-*/gpt-* 名字；
	// 未知模型直接回退到目录里的第一个可用模型，避免客户端完全不可用。
	log.Printf("WARN 未知模型 %q，回退到 %s（可用：%s）", want, catalog[0].ID, strings.Join(ids, ", "))
	return catalog[0], nil
}

var effortOrder = []string{"none", "instant", "minimal", "low", "medium", "high", "xhigh", "max"}

// pickReasoning 把请求强度映射到模型真实支持的档位。
func pickReasoning(model capy.Model, want, fallback string) string {
	if len(model.Efforts) == 0 {
		return ""
	}
	candidate := strings.ToLower(strings.TrimSpace(want))
	if candidate == "" {
		candidate = strings.ToLower(strings.TrimSpace(fallback))
	}
	if candidate == "" {
		if model.DefaultEffort != "" {
			return model.DefaultEffort
		}
		return model.Efforts[0]
	}
	for _, e := range model.Efforts {
		if strings.EqualFold(e, candidate) {
			return e
		}
	}
	idx := indexOf(effortOrder, candidate)
	if idx < 0 {
		if model.DefaultEffort != "" {
			return model.DefaultEffort
		}
		return model.Efforts[0]
	}
	best, bestDist := model.Efforts[0], 1<<30
	for _, e := range model.Efforts {
		j := indexOf(effortOrder, e)
		if j < 0 {
			continue
		}
		d := j - idx
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist = e, d
		}
	}
	return best
}

func indexOf(list []string, v string) int {
	for i, item := range list {
		if item == v {
			return i
		}
	}
	return -1
}

func containsFold(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), v) {
			return true
		}
	}
	return false
}

func clientLabel(r *http.Request) string {
	ua := r.Header.Get("user-agent")
	if len(ua) > 60 {
		ua = ua[:60]
	}
	return ua
}

// handlePanel 提供内嵌面板静态资源。
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/panel/")
	asset, ok := panel.Lookup(path)
	if !ok {
		asset, ok = panel.Lookup("index.html")
	}
	if !ok {
		http.Error(w, "panel asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", asset.ContentType)
	w.Header().Set("cache-control", "no-store")
	_, _ = w.Write(asset.Body)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": Version,
		"uptime":  int(time.Since(s.start).Seconds()),
	})
}

func (s *Server) handleUnsupported(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, http.StatusBadRequest, "Capy 桥接只提供 chat completions / messages / responses", "unsupported_endpoint")
}
