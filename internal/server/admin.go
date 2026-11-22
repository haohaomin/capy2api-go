package server

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/capyapp"
	"github.com/haohaomin/capy2api/internal/config"
	"github.com/haohaomin/capy2api/internal/store"
)

// ------------------------------------------------------------------ 登录

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	if body.Key == "" || subtleCompare(body.Key, s.cfg().APIKey) != true {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "密钥不正确"}})
		return
	}
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.signSession(exp),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ 概览

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	accounts := s.store.List()
	total, ok, failed, avg := s.log.Stats()
	_, promptTokens, outputTokens, cacheTokens := s.log.Totals()
	cacheRatio := 0.0
	if promptTokens > 0 {
		cacheRatio = float64(cacheTokens) / float64(promptTokens)
	}
	ready, invalid := 0, 0
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		switch a.State {
		case "ok":
			ready++
		case "invalid":
			invalid++
		default:
			ready++
		}
	}
	modelCount := 0
	modelSample := ""
	if len(accounts) > 0 {
		if acc, found := s.store.Raw(accounts[0].ID); found {
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			if models, err := s.catalog(ctx, acc, false); err == nil {
				modelCount = len(models)
				if len(models) > 0 {
					modelSample = models[0].ID
				}
			}
			cancel()
		}
	}
	base := "http://" + s.cfg().Listen
	if strings.HasPrefix(base, "http://0.0.0.0") {
		base = strings.Replace(base, "0.0.0.0", "127.0.0.1", 1)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          Version,
		"mode":             s.cfg().Capy.Mode,
		"template_jam":     s.cfg().Capy.TemplateJam,
		"project_id":       s.cfg().Capy.ProjectID,
		"uptime_sec":       int(time.Since(s.start).Seconds()),
		"listen":           s.cfg().Listen,
		"config":           s.cfg().Path(),
		"accounts_total":   len(accounts),
		"accounts_ready":   ready,
		"accounts_invalid": invalid,
		"models_total":     modelCount,
		"requests": map[string]any{
			"total": total, "ok": ok, "failed": failed, "avg_ms": avg,
		},
		"tokens": map[string]any{
			"prompt": promptTokens, "completion": outputTokens,
			"cache_read": cacheTokens, "cache_hit_ratio": cacheRatio,
		},
		"connect": map[string]any{
			"openai_base":    base + "/v1",
			"anthropic_base": base,
			"api_key_hint":   maskKey(s.cfg().APIKey),
			"model_sample":   modelSample,
		},
		"capy_app": map[string]any{
			"available": capyapp.Available(),
			"running":   capyapp.AppRunning(),
			"path":      capyapp.Dir(),
		},
		"recent": s.log.Recent(12),
	})
}

func maskKey(k string) string {
	if len(k) <= 10 {
		return "已配置"
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// ------------------------------------------------------------------ 账号

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.store.List()})
}

func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		Cookie    string `json:"cookie"`
		SessionID string `json:"session_id"`
		OrgID     string `json:"org_id"`
		DeviceID  string `json:"device_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	if strings.TrimSpace(body.Cookie) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "cookie 不能为空"}})
		return
	}
	acc := store.Account{
		Name:      strings.TrimSpace(body.Name),
		Cookie:    strings.TrimSpace(body.Cookie),
		SessionID: strings.TrimSpace(body.SessionID),
		OrgID:     strings.TrimSpace(body.OrgID),
		DeviceID:  strings.TrimSpace(body.DeviceID),
		Source:    "manual",
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	acc, models, err := s.validateAccount(ctx, acc)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "校验失败：" + err.Error()}})
		return
	}
	saved, err := s.store.Add(acc)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.store.MarkState(saved.ID, "ok", "")
	s.invalidateModels(saved.ID)
	s.ensureDaemon(saved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": saved.ID, "name": saved.Name, "models": models})
}

func (s *Server) handleAccountImport(w http.ResponseWriter, r *http.Request) {
	if !capyapp.Available() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "没有找到本机 Capy 登录态：先打开 Capy 桌面端并登录",
		}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	detected, err := capyapp.Detect(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	var lastErr error
	var saved store.Account
	var modelIDs []string
	for _, org := range detected.Orgs {
		acc := store.Account{
			Cookie:    detected.Cookie,
			SessionID: detected.SessionID,
			UserID:    detected.UserID,
			OrgID:     org.OrgID,
			DeviceID:  org.DeviceID,
			Source:    "capy-app",
		}
		validated, models, err := s.validateAccount(ctx, acc)
		if err != nil {
			lastErr = err
			continue
		}
		record, err := s.store.Add(validated)
		if err != nil {
			lastErr = err
			continue
		}
		s.store.MarkState(record.ID, "ok", "")
		s.invalidateModels(record.ID)
		s.ensureDaemon(record)
		saved, modelIDs = record, models
		break
	}
	if saved.ID == "" {
		msg := "导入失败"
		if lastErr != nil {
			msg += "：" + lastErr.Error()
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": msg}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": saved.ID, "name": saved.Name, "email": saved.Email,
		"org_id": saved.OrgID, "device_id": saved.DeviceID, "models": modelIDs,
	})
}

func (s *Server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled       *bool   `json:"enabled"`
		Name          *string `json:"name"`
		DeviceID      *string `json:"device_id"`
		OrgID         *string `json:"org_id"`
		Mode          *string `json:"mode"`
		ProjectID     *string `json:"project_id"`
		WorkspaceMode *string `json:"workspace_mode"`
		WorkspaceRoot *string `json:"workspace_root"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	acc, err := s.store.Update(id, func(a *store.Account) {
		if body.Enabled != nil {
			a.Enabled = *body.Enabled
		}
		if body.Name != nil {
			a.Name = strings.TrimSpace(*body.Name)
		}
		if body.DeviceID != nil {
			a.DeviceID = strings.TrimSpace(*body.DeviceID)
		}
		if body.OrgID != nil {
			a.OrgID = strings.TrimSpace(*body.OrgID)
		}
		if body.Mode != nil {
			m := strings.ToLower(strings.TrimSpace(*body.Mode))
			if m == "web" || m == "device" {
				a.Mode = m
			}
		}
		if body.ProjectID != nil {
			a.ProjectID = strings.TrimSpace(*body.ProjectID)
		}
		if body.WorkspaceMode != nil {
			a.WorkspaceMode = strings.TrimSpace(*body.WorkspaceMode)
		}
		if body.WorkspaceRoot != nil {
			a.WorkspaceRoot = strings.TrimSpace(*body.WorkspaceRoot)
		}
	})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.invalidateModels(acc.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Redact()})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.Delete(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.invalidateModels(id)
	s.sessMu.Lock()
	delete(s.sess, id)
	s.sessMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAccountProbe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acc, found := s.store.Raw(id)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "账号不存在"}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	validated, models, err := s.validateAccount(ctx, acc)
	if err != nil {
		s.store.MarkState(id, "invalid", err.Error())
		s.invalidateModels(id)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	if _, err := s.store.Update(id, func(a *store.Account) {
		a.Email = validated.Email
		a.UserID = validated.UserID
		a.OrgID = validated.OrgID
		a.OrgName = validated.OrgName
		if a.DeviceID == "" {
			a.DeviceID = validated.DeviceID
		}
		if a.Name == "" || a.Name == a.ID {
			a.Name = firstNonEmpty(validated.Email, a.Name)
		}
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.store.MarkState(id, "ok", "")
	s.invalidateModels(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
}

// validateAccount 用一份临时会话验证 cookie/org，并补全邮箱、org、device。
func (s *Server) validateAccount(ctx context.Context, acc store.Account) (store.Account, []string, error) {
	sess := &capy.Session{Client: s.client, Cookie: acc.Cookie, SessionID: acc.SessionID}
	me, err := sess.Me(ctx)
	if err != nil {
		return acc, nil, err
	}
	if email, ok := me["email"].(string); ok {
		acc.Email = email
	}
	if id, ok := me["id"].(string); ok {
		acc.UserID = id
	}
	orgs, err := sess.Organizations(ctx)
	if err != nil {
		return acc, nil, err
	}
	if len(orgs) == 0 {
		return acc, nil, errNoOrg
	}
	chosen := orgs[0]
	if acc.OrgID != "" {
		for _, o := range orgs {
			if o.ID == acc.OrgID {
				chosen = o
			}
		}
	}
	acc.OrgID, acc.OrgName = chosen.ID, chosen.Name
	if acc.DeviceID == "" {
		host, _ := os.Hostname()
		if _, devID, _, token, eerr := sess.EnrollDevice(ctx, "capy2api", host, runtime.GOOS, runtime.GOARCH); eerr == nil {
			acc.DeviceID, acc.DeviceToken = devID, token
		}
	}
	if acc.DeviceID == "" {
		if detected, err := capyapp.Detect(ctx); err == nil {
			for _, o := range detected.Orgs {
				if o.OrgID == acc.OrgID && o.DeviceID != "" {
					acc.DeviceID = o.DeviceID
					break
				}
			}
			if acc.DeviceID == "" && len(detected.Orgs) == 1 {
				acc.DeviceID = detected.Orgs[0].DeviceID
			}
		}
	}
	models, err := sess.Models(ctx, acc.OrgID)
	if err != nil {
		return acc, nil, err
	}
	ids := make([]string, 0, 4)
	for _, m := range models {
		if m.FirstParty() {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return acc, nil, errNoFreeModel
	}
	if acc.Name == "" {
		acc.Name = firstNonEmpty(acc.Email, acc.UserID, acc.ID)
	}
	acc.Enabled = true
	return acc, ids, nil
}

var (
	errNoOrg       = &capyError{"cookie 下没有组织"}
	errNoFreeModel = &capyError{"该账号没有可用的免费广告模型"}
)

type capyError struct{ msg string }

func (e *capyError) Error() string { return e.msg }

// ------------------------------------------------------------------ 模型 / 配置 / 日志

func (s *Server) handlePanelModels(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	accs := s.store.List()
	if len(accs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{}})
		return
	}
	seen := map[string]capy.Model{}
	for _, view := range accs {
		if !view.Enabled {
			continue
		}
		acc, found := s.store.Raw(view.ID)
		if !found {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		models, err := s.catalog(ctx, acc, refresh)
		cancel()
		if err != nil {
			continue
		}
		for _, m := range models {
			seen[m.ID] = m
		}
	}
	items := make([]map[string]any, 0, len(seen))
	for _, m := range seen {
		items = append(items, map[string]any{
			"id": m.ID, "name": m.Name, "provider": m.Provider, "limits": m.Limits,
			"efforts": m.Efforts, "default_effort": m.DefaultEffort, "pricing": m.Pricing,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["id"].(string) < items[j]["id"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"models": items})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"path": s.cfg().Path(), "config": map[string]any{
		"api_key":              s.cfg().APIKey,
		"mode":                 s.cfg().Capy.Mode,
		"template_jam":         s.cfg().Capy.TemplateJam,
		"project_id":           s.cfg().Capy.ProjectID,
		"reasoning":            s.cfg().Capy.Reasoning,
		"turn_timeout_sec":     s.cfg().Capy.TurnTimeoutSec,
		"poll_interval_ms":     s.cfg().Capy.PollIntervalMS,
		"max_concurrent":       s.cfg().Capy.MaxConcurrent,
		"delete_after":         s.cfg().Capy.DeleteAfter,
		"typewriter":           s.cfg().Capy.Typewriter,
		"realtime":             s.cfg().Capy.Realtime,
		"workspace_mode":       s.cfg().Capy.Workspace.Mode,
		"workspace_root":       s.cfg().Capy.Workspace.Root,
		"allowed_models":       s.cfg().Capy.AllowedModels,
		"device_policy":        s.cfg().Capy.DevicePolicy,
		"queue_timeout_sec":    s.cfg().Capy.QueueTimeoutSec,
		"max_tool_steps":       s.cfg().Capy.MaxToolSteps,
		"conversation_ttl_sec": s.cfg().Capy.ConversationTTLSec,
		"listen":               s.cfg().Listen,
		"api_base":             s.cfg().Capy.APIBase,
	}})
}

func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey             *string  `json:"api_key"`
		Mode               *string  `json:"mode"`
		TemplateJam        *string  `json:"template_jam"`
		ProjectID          *string  `json:"project_id"`
		Reasoning          *string  `json:"reasoning"`
		TurnTimeoutSec     *int     `json:"turn_timeout_sec"`
		PollIntervalMS     *int     `json:"poll_interval_ms"`
		MaxConcurrent      *int     `json:"max_concurrent"`
		DeleteAfter        *bool    `json:"delete_after"`
		Typewriter         *bool    `json:"typewriter"`
		Realtime           *bool    `json:"realtime"`
		ThreadMode         *string  `json:"thread_mode"`
		PoolSize           *int     `json:"pool_size"`
		MemoryGuard        *bool    `json:"memory_guard"`
		ReuseConversation  *bool    `json:"reuse_conversation"`
		WorkspaceMode      *string  `json:"workspace_mode"`
		WorkspaceRoot      *string  `json:"workspace_root"`
		AllowedModels      []string `json:"allowed_models"`
		ConversationTTLSec *int     `json:"conversation_ttl_sec"`
		DevicePolicy       *string  `json:"device_policy"`
		QueueTimeoutSec    *int     `json:"queue_timeout_sec"`
		MaxToolSteps       *int     `json:"max_tool_steps"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	cfg := *s.cfg()
	if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		cfg.APIKey = strings.TrimSpace(*body.APIKey)
	}
	if body.Mode != nil {
		m := strings.ToLower(strings.TrimSpace(*body.Mode))
		if m == "thread" || m == "web" || m == "device" {
			cfg.Capy.Mode = m
		}
	}
	if body.TemplateJam != nil {
		cfg.Capy.TemplateJam = strings.TrimSpace(*body.TemplateJam)
	}
	if body.ProjectID != nil {
		cfg.Capy.ProjectID = strings.TrimSpace(*body.ProjectID)
	}
	if body.Reasoning != nil {
		cfg.Capy.Reasoning = strings.TrimSpace(*body.Reasoning)
	}
	if body.TurnTimeoutSec != nil && *body.TurnTimeoutSec >= 30 {
		cfg.Capy.TurnTimeoutSec = *body.TurnTimeoutSec
	}
	if body.PollIntervalMS != nil && *body.PollIntervalMS >= 200 {
		cfg.Capy.PollIntervalMS = *body.PollIntervalMS
	}
	if body.MaxConcurrent != nil && *body.MaxConcurrent > 0 {
		cfg.Capy.MaxConcurrent = *body.MaxConcurrent
	}
	if body.DeleteAfter != nil {
		cfg.Capy.DeleteAfter = *body.DeleteAfter
	}
	if body.Typewriter != nil {
		cfg.Capy.Typewriter = *body.Typewriter
	}
	if body.Realtime != nil {
		cfg.Capy.Realtime = *body.Realtime
	}
	if body.ThreadMode != nil {
		m := strings.ToLower(strings.TrimSpace(*body.ThreadMode))
		if m == "root" || m == "side" {
			cfg.Capy.ThreadMode = m
		}
	}
	if body.PoolSize != nil && *body.PoolSize > 0 {
		cfg.Capy.PoolSize = *body.PoolSize
	}
	if body.MemoryGuard != nil {
		cfg.Capy.MemoryGuard = *body.MemoryGuard
	}
	if body.WorkspaceMode != nil && (*body.WorkspaceMode == "worktree" || *body.WorkspaceMode == "linked") {
		cfg.Capy.Workspace.Mode = *body.WorkspaceMode
	}
	if body.WorkspaceRoot != nil && strings.TrimSpace(*body.WorkspaceRoot) != "" {
		root, err := config.ExpandHome(strings.TrimSpace(*body.WorkspaceRoot))
		if err == nil {
			cfg.Capy.Workspace.Root = root
		}
	}
	if body.DevicePolicy != nil {
		p := strings.ToLower(strings.TrimSpace(*body.DevicePolicy))
		if p == "text" || p == "readonly" || p == "all" {
			cfg.Capy.DevicePolicy = p
		}
	}
	if body.QueueTimeoutSec != nil {
		cfg.Capy.QueueTimeoutSec = *body.QueueTimeoutSec
	}
	if body.MaxToolSteps != nil && *body.MaxToolSteps > 0 {
		cfg.Capy.MaxToolSteps = *body.MaxToolSteps
	}
	if body.ConversationTTLSec != nil && *body.ConversationTTLSec > 0 {
		cfg.Capy.ConversationTTLSec = *body.ConversationTTLSec
		s.convos.setTTL(time.Duration(*body.ConversationTTLSec) * time.Second)
	}
	if body.AllowedModels != nil {
		cfg.Capy.AllowedModels = body.AllowedModels
	}
	if err := cfg.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": "保存配置失败: " + err.Error()}})
		return
	}
	s.cfgPtr.Store(&cfg)
	if body.AllowedModels != nil {
		// 模型白名单变了：清掉目录缓存，下一次 /v1/models 立刻按新名单过滤。
		s.modelMu.Lock()
		s.models = map[string]modelsCache{}
		s.modelMu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": body.MaxConcurrent != nil})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.log.Recent(limit)})
}

func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"available": capyapp.Available(),
		"running":   capyapp.AppRunning(),
		"path":      capyapp.Dir(),
	})
}

func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// handleStats 返回按模型聚合的请求统计（面板「模型」页用）。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	since, models, total := statsSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"since":  since.Format(time.RFC3339),
		"total":  total,
		"models": models,
	})
}

// handleConversations 列出被复用的会话线程（面板用）。
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       s.cfg().Capy.ReuseConversation,
		"ttl_sec":       s.cfg().Capy.ConversationTTLSec,
		"conversations": s.convos.list(),
	})
}

// handleConversationsCleanup 立即清理全部会话线程（面板手动触发）。
func (s *Server) handleConversationsCleanup(w http.ResponseWriter, r *http.Request) {
	items := s.convos.list()
	deleted := 0
	for _, c := range items {
		acc, ok := s.store.Raw(c.AccountID)
		if ok {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			sess := s.sessionFor(acc)
			_, _ = sess.DeleteJams(ctx, acc.OrgID, []string{c.JamID})
			cancel()
			deleted++
		}
		s.convos.drop(c.Key)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "remaining": len(s.convos.list())})
}

// ---------------------------------------------------------------- 订阅 key

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": s.keys.List()})
}

func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string   `json:"name"`
		QuotaRequests int64    `json:"quota_requests"`
		QuotaTokens   int64    `json:"quota_tokens"`
		RPMLimit      int      `json:"rpm_limit"`
		MaxConcurrent int      `json:"max_concurrent"`
		AllowedModels []string `json:"allowed_models"`
		AccountID     string   `json:"account_id"`
		Note          string   `json:"note"`
		TTLDays       int      `json:"ttl_days"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	k := store.APIKey{
		Name: body.Name, QuotaRequests: body.QuotaRequests, QuotaTokens: body.QuotaTokens,
		RPMLimit: body.RPMLimit, MaxConcurrent: body.MaxConcurrent,
		AllowedModels: body.AllowedModels, AccountID: body.AccountID, Note: body.Note,
	}
	if body.TTLDays > 0 {
		k.ExpiresAt = time.Now().AddDate(0, 0, body.TTLDays)
	}
	created, err := s.keys.Create(k)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	// 明文只在这里返回一次。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": created.Key, "id": created.ID, "name": created.Name})
}

func (s *Server) handleKeyPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled       *bool    `json:"enabled"`
		Name          *string  `json:"name"`
		QuotaRequests *int64   `json:"quota_requests"`
		QuotaTokens   *int64   `json:"quota_tokens"`
		RPMLimit      *int     `json:"rpm_limit"`
		AllowedModels []string `json:"allowed_models"`
		AccountID     *string  `json:"account_id"`
		Note          *string  `json:"note"`
		ResetUsage    *bool    `json:"reset_usage"`
		TTLDays       *int     `json:"ttl_days"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	updated, err := s.keys.Update(id, func(k *store.APIKey) {
		if body.Enabled != nil {
			k.Enabled = *body.Enabled
		}
		if body.Name != nil {
			k.Name = *body.Name
		}
		if body.QuotaRequests != nil {
			k.QuotaRequests = *body.QuotaRequests
		}
		if body.QuotaTokens != nil {
			k.QuotaTokens = *body.QuotaTokens
		}
		if body.RPMLimit != nil {
			k.RPMLimit = *body.RPMLimit
		}
		if body.AllowedModels != nil {
			k.AllowedModels = body.AllowedModels
		}
		if body.AccountID != nil {
			k.AccountID = *body.AccountID
		}
		if body.Note != nil {
			k.Note = *body.Note
		}
		if body.ResetUsage != nil && *body.ResetUsage {
			k.Requests, k.PromptTokens, k.CompletionTokens = 0, 0, 0
		}
		if body.TTLDays != nil {
			if *body.TTLDays > 0 {
				k.ExpiresAt = time.Now().AddDate(0, 0, *body.TTLDays)
			} else {
				k.ExpiresAt = time.Time{}
			}
		}
	})
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": updated.Redact()})
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.keys.Delete(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
