// conversation.go 会话复用：同一段对话的第二轮起，复用同一条上游线程。
//
// 为什么这么做（对齐 WorkBuddy2API 的会话粘性 + prompt_cache_key 思路）：
//   - 无状态转述（每次把整段 history 塞进新线程）会让上游缓存只能命中静态前缀，
//     对话越长越亏；
//   - 复用同一条线程时，上下文留在上游，前缀缓存天然连续增长，输入 token 也更少；
//   - 会话键优先取客户端声明的 conversation_id；没有就用「system + 首条 user」的内容
//     哈希（不能退化成 user_id，否则同一用户的多段对话会互相覆盖）。
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/haohaomin/capy2api/internal/store"
)

type convEntry struct {
	jamID      string
	accountID  string
	orgID      string
	model      string
	systemHash string
	inflight   string // 正在处理的那条用户消息（用于识别「重试」还是「新消息」）
	failures   int    // 连续超时次数：到 2 次就解绑，下一轮重建线程
	superseded bool   // 已被同一会话的新消息接管（旧请求收尾时不要解绑/覆盖绑定）
	sent       int    // 已经投递给上游的消息条数（用于只发增量，保证工具结果不丢）
	last       time.Time
	lock       chan struct{}
}

type convRegistry struct {
	mu   sync.Mutex
	by   map[string]*convEntry
	ttl  time.Duration
	max  int
	path string // conversations.json（重启后清理遗留线程用）
}

// setTTL 热更新会话存活时间（面板改配置后立即生效）。
func (r *convRegistry) setTTL(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	r.mu.Lock()
	r.ttl = ttl
	r.mu.Unlock()
}

func newConvRegistry(ttl time.Duration, max int, path string) *convRegistry {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if max <= 0 {
		max = 256
	}
	r := &convRegistry{by: map[string]*convEntry{}, ttl: ttl, max: max, path: path}
	r.load()
	return r
}

// load 读回上次进程留下的会话线程（启动后统一回收，避免线程泄漏）。
func (r *convRegistry) load() {
	if r.path == "" {
		return
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var payload struct {
		Entries []struct {
			Key       string    `json:"key"`
			JamID     string    `json:"jam_id"`
			AccountID string    `json:"account_id"`
			OrgID     string    `json:"org_id"`
			LastUsed  time.Time `json:"last_used"`
		} `json:"entries"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return
	}
	for _, e := range payload.Entries {
		if e.JamID == "" {
			continue
		}
		r.by[e.Key] = &convEntry{jamID: e.JamID, accountID: e.AccountID, orgID: e.OrgID, last: e.LastUsed, lock: make(chan struct{}, 1)}
	}
}

// save 落盘当前绑定（条目少，直接整体写）。
func (r *convRegistry) save() {
	if r.path == "" {
		return
	}
	r.mu.Lock()
	payload := struct {
		Entries []map[string]any `json:"entries"`
	}{}
	for k, e := range r.by {
		if e.jamID == "" {
			continue
		}
		payload.Entries = append(payload.Entries, map[string]any{
			"key": k, "jam_id": e.jamID, "account_id": e.accountID, "org_id": e.orgID, "last_used": e.last,
		})
	}
	r.mu.Unlock()
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.path)
}

// stale 返回上次进程遗留、需要回收的线程（启动时调用一次）。
func (r *convRegistry) stale() []convInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]convInfo, 0, len(r.by))
	for k, e := range r.by {
		if e.jamID != "" {
			out = append(out, convInfo{Key: k, JamID: e.jamID, AccountID: e.accountID})
		}
	}
	return out
}

// acquire 取会话槽位；返回的 release 必须调用。busy=true 表示同一会话已有请求在跑，
// 调用方应退回无状态路径，而不是排队。
// acquireCtx 在 wait 时间内等待同一会话的锁；拿不到才返回 busy。
// 这样并发请求会「排队等前一轮结束」，而不是立刻新建线程重发全量历史。
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func (r *convRegistry) acquireCtx(ctx context.Context, key string, wait time.Duration) (*convEntry, func(), bool) {
	deadline := time.Now().Add(wait)
	for {
		ent, release, busy := r.acquire(key)
		if !busy {
			return ent, release, false
		}
		if time.Now().After(deadline) {
			return ent, release, true
		}
		select {
		case <-ctx.Done():
			return ent, release, true
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *convRegistry) acquire(key string) (e *convEntry, release func(), busy bool) {
	r.mu.Lock()
	ent := r.by[key]
	if ent == nil {
		if len(r.by) >= r.max {
			r.mu.Unlock()
			return nil, func() {}, true
		}
		ent = &convEntry{lock: make(chan struct{}, 1)}
		r.by[key] = ent
	}
	r.mu.Unlock()
	select {
	case ent.lock <- struct{}{}:
		return ent, func() { <-ent.lock }, false
	default:
		// busy 时也把 entry 带回去：调用方需要看 inflight/jamID 才能判断是重试还是抢占。
		return ent, func() {}, true
	}
}

// bind 记录会话与线程的绑定（新建线程成功后调用）。
func (r *convRegistry) bind(key, jamID, accountID, orgID, model, systemHash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ent := r.by[key]
	if ent == nil {
		ent = &convEntry{lock: make(chan struct{}, 1)}
		r.by[key] = ent
	}
	ent.jamID, ent.accountID, ent.orgID = jamID, accountID, orgID
	ent.model, ent.systemHash, ent.last = model, systemHash, time.Now()
	ent.superseded = false
	r.mu.Unlock()
	r.save()
	r.mu.Lock()
}

func (r *convRegistry) touch(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ent := r.by[key]; ent != nil {
		ent.last = time.Now()
	}
}

// noteFailure 记录一次连续失败；默认累计到 2 次才解绑（WorkBuddy 的「失效即解绑」在这里放宽，
// 避免偶发超时就把还活着的线程丢掉）。hard=true 时立即解绑（线程不存在/账号失效）。
func (r *convRegistry) noteFailure(key string, hard bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ent := r.by[key]
	if ent == nil {
		return false
	}
	ent.failures++
	if hard || ent.failures >= 2 {
		delete(r.by, key)
		return true
	}
	return false
}

// markSuperseded 标记某条线程已被新消息接管。
func (r *convRegistry) markSuperseded(key, jamID string) {
	r.mu.Lock()
	if ent := r.by[key]; ent != nil && ent.jamID == jamID {
		ent.superseded = true
	}
	r.mu.Unlock()
}

// isSuperseded 判断会话当前绑定是否已被接管。
func (r *convRegistry) isSuperseded(key, jamID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ent := r.by[key]
	return ent != nil && ent.superseded && ent.jamID == jamID
}

// sentCount 返回该会话已投递的消息条数。
func (r *convRegistry) sentCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ent := r.by[key]; ent != nil {
		return ent.sent
	}
	return 0
}

// setSentCount 记录已投递的消息条数。
func (r *convRegistry) setSentCount(key string, n int) {
	r.mu.Lock()
	if ent := r.by[key]; ent != nil {
		ent.sent = n
	}
	r.mu.Unlock()
}

// currentJam 返回会话当前绑定的线程（没有则空）。
func (r *convRegistry) currentJam(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ent := r.by[key]; ent != nil {
		return ent.jamID
	}
	return ""
}

// dropIf 仅当会话仍绑在指定 jam 上时才解绑（避免旧请求失败时误删新请求刚绑定的线程）。
func (r *convRegistry) dropIf(key, jamID string) {
	if jamID == "" {
		return
	}
	r.mu.Lock()
	if ent := r.by[key]; ent != nil && ent.jamID != jamID {
		r.mu.Unlock()
		return
	}
	delete(r.by, key)
	r.mu.Unlock()
	r.save()
}

// drop 解绑（上游报错/线程没了时调用）。
func (r *convRegistry) drop(key string) {
	r.mu.Lock()
	delete(r.by, key)
	r.mu.Unlock()
	r.save()
}

type convInfo struct {
	Key       string    `json:"key"`
	JamID     string    `json:"jam_id"`
	AccountID string    `json:"account_id"`
	LastUsed  time.Time `json:"last_used"`
	Failures  int       `json:"failures"`
}

func (r *convRegistry) list() []convInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]convInfo, 0, len(r.by))
	for k, e := range r.by {
		if e.jamID == "" {
			continue
		}
		out = append(out, convInfo{Key: k, JamID: e.jamID, AccountID: e.accountID, LastUsed: e.last, Failures: e.failures})
	}
	return out
}

// gc 清理空闲超时的会话线程。
func (r *convRegistry) gc(ctx context.Context, lookup func(id string) (store.Account, bool), del func(ctx context.Context, acc store.Account, jamID string)) {
	now := time.Now()
	type entry struct {
		key, jamID, account string
		last                time.Time
	}
	r.mu.Lock()
	ttl, max := r.ttl, r.max
	all := make([]entry, 0, len(r.by))
	for k, e := range r.by {
		all = append(all, entry{key: k, jamID: e.jamID, account: e.accountID, last: e.last})
	}
	r.mu.Unlock()

	// 最新的排前面：超出 max 的旧条目按「淘汰」处理，和过期条目一起删。
	sort.Slice(all, func(i, j int) bool { return all[i].last.After(all[j].last) })
	for i, e := range all {
		expired := e.jamID != "" && now.Sub(e.last) > ttl
		overflow := max > 0 && i >= max
		if !expired && !overflow {
			continue
		}
		if e.jamID != "" {
			if acc, ok := lookup(e.account); ok {
				del(ctx, acc, e.jamID)
			}
		}
		r.drop(e.key)
	}
}

// conversationKey 解析会话键：客户端声明的 conversation_id 优先，否则用内容派生的稳定键。
func conversationKey(req *chatRequest) (key, systemHash string, declared bool) {
	systemHash = hashSystem(req.Messages)
	if v := declaredConversationID(req); v != "" {
		return "conv:" + v, systemHash, true
	}
	// 内容派生：system + 首条 user（同一段对话稳定，不同对话不同）。
	first := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			if text, err := renderContent(m.Content); err == nil {
				first = text
			}
			break
		}
	}
	if strings.TrimSpace(first) == "" {
		return "", systemHash, false
	}
	sum := sha256.Sum256([]byte(systemHash + "\x00" + first))
	return "derived:" + hex.EncodeToString(sum[:12]), systemHash, false
}

func declaredConversationID(req *chatRequest) string {
	for _, v := range []string{req.ConversationID, req.ConversationIDCamel, req.PromptCacheKey} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if len(req.Metadata) > 0 {
		var meta map[string]any
		if json.Unmarshal(req.Metadata, &meta) == nil {
			for _, k := range []string{"conversation_id", "conversationId"} {
				if v, ok := meta[k].(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return ""
}

func hashSystem(messages []chatMessage) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		if text, err := renderContent(m.Content); err == nil {
			b.WriteString(text)
			b.WriteString("\x00")
		}
	}
	if b.Len() == 0 {
		return "nosys"
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// conversationFor 决定这次请求是否复用已有线程。
//
// 返回 (账号, 复用的 jam, 会话键, release, systemHash)。release 必须调用。
// 复用时账号会被固定到绑定的那个（上游前缀缓存按账号隔离，换号等于全量 miss）。
func (s *Server) conversationFor(ctx context.Context, req *chatRequest, acc store.Account, tenantID string) (store.Account, string, string, func(), string, bool) {
	noop := func() {}
	// 工具请求同样复用线程：否则 Claude Code 这类每请求都带 tools 的客户端会每轮新建线程，
	// 白付 2 秒建线程、丢掉 prompt cache。
	if !s.cfg().Capy.ReuseConversation {
		return acc, "", "", noop, "", false
	}
	mode := strings.ToLower(firstNonEmpty(acc.Mode, s.cfg().Capy.Mode, "thread"))
	if mode != "thread" {
		return acc, "", "", noop, "", false
	}
	key, sysHash, declared := conversationKey(req)
	if key != "" {
		// 租户隔离：不同 key 即使 conversation_id 相同也不能共用线程。
		key = tenantID + "|" + key
	}
	// 没有显式会话 id、且只发了一条消息：当作一次性请求，不复用（避免把多次独立提问
	// 串成同一段对话）。发完整 history 的客户端（≥3 条）才用内容派生键复用。
	if key == "" || (!declared && len(req.Messages) < 3) {
		return acc, "", "", noop, sysHash, false
	}
	lastText, _ := lastUserText(req.Messages)
	msgHash := hashText(lastText)
	// 先短等一下（前一轮可能正好收尾）。
	ent, release, busy := s.convos.acquireCtx(ctx, key, 1500*time.Millisecond)
	if busy && !declared {
		// 内容派生的会话键只是「尽量复用」：同一句话可能来自另一个新会话，
		// 不能抢占、也不该判重试，直接新建线程即可。
		return acc, "", "", noop, sysHash, false
	}
	if busy {
		same := false
		var inflightJam, inflightAcc string
		if ent != nil {
			s.convos.mu.Lock()
			same = ent.inflight != "" && ent.inflight == msgHash
			inflightJam, inflightAcc = ent.jamID, ent.accountID
			s.convos.mu.Unlock()
		}
		if same {
			log.Printf("会话 %s：检测到相同消息重试，等待 10 秒", key)
			// 完全相同的重试：再等 10 秒，还不结束就告诉客户端别重复跑。
			ent2, rel2, stillBusy := s.convos.acquireCtx(ctx, key, 10*time.Second)
			log.Printf("会话 %s：相同重试等待结束 busy=%v", key, stillBusy)
			if !stillBusy {
				s.convos.mu.Lock()
				ent2.inflight = msgHash
				s.convos.mu.Unlock()
				return acc, ent2.jamID, key, rel2, sysHash, false
			}
			return acc, "", "", noop, sysHash, true
		}
		log.Printf("会话 %s：检测到新消息抢占（旧 jam=%s）", key, inflightJam)
		// 不同的新消息：打断正在跑的那一轮，然后**复用它同一条线程**继续。
		waitUntil := time.Now().Add(5 * time.Second)
		for inflightJam == "" && time.Now().Before(waitUntil) {
			time.Sleep(200 * time.Millisecond)
			if ent != nil {
				s.convos.mu.Lock()
				inflightJam, inflightAcc = ent.jamID, ent.accountID
				s.convos.mu.Unlock()
			}
		}
		if inflightJam != "" {
			// 先标记「已被接管」：旧请求收尾时不要解绑、也不要覆盖绑定。
			s.convos.markSuperseded(key, inflightJam)
			if bound, ok := s.store.Raw(inflightAcc); ok && bound.Enabled {
				sess := s.sessionFor(bound)
				ictx, cancel := context.WithTimeout(ctx, 8*time.Second)
				if err := sess.Interrupt(ictx, bound.OrgID, inflightJam); err == nil {
					log.Printf("会话 %s：新消息抢占，已打断 jam %s（复用同一线程）", key, inflightJam)
				} else {
					log.Printf("WARN 会话 %s 抢占打断失败: %v", key, err)
				}
				cancel()
			}
			// 等旧请求把锁放开（打断后通常 1–2 秒），然后复用同一条线程。
			ent2, rel2, stillBusy := s.convos.acquireCtx(ctx, key, 10*time.Second)
			log.Printf("会话 %s：相同重试等待结束 busy=%v", key, stillBusy)
			if !stillBusy {
				s.convos.mu.Lock()
				if ent2.jamID == inflightJam {
					ent2.inflight = msgHash
					ent2.failures = 0
					ent2.superseded = false
					acc2, ok := s.store.Raw(ent2.accountID)
					s.convos.mu.Unlock()
					if ok {
						return acc2, inflightJam, key, rel2, sysHash, false
					}
					rel2()
					return acc, "", "", noop, sysHash, false
				}
				s.convos.mu.Unlock()
				rel2()
			}
		}
		return acc, "", "", noop, sysHash, false
	}
	s.convos.mu.Lock()
	ent.inflight = msgHash
	s.convos.mu.Unlock()
	if ent.jamID == "" {
		return acc, "", key, release, sysHash, false
	}
	bound, ok := s.store.Raw(ent.accountID)
	if !ok || !bound.Enabled {
		s.convos.drop(key)
		return acc, "", key, release, sysHash, false
	}
	// 刚用过的线程不做探活（省一次上游 RTT）；只有闲置较久才校验是否还活着。
	if time.Since(ent.last) < 90*time.Second {
		return bound, ent.jamID, key, release, sysHash, false
	}
	// 线程可能已被上游/回收任务删掉：先探活，失效就解绑重建。
	sess := s.sessionFor(bound)
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, jerr := sess.GetJam(checkCtx, bound.OrgID, ent.jamID)
	cancel()
	if jerr != nil {
		s.convos.drop(key)
		return acc, "", key, release, sysHash, false
	}
	return bound, ent.jamID, key, release, sysHash, false
}

// lastUserText 取最后一条 user 消息（复用线程时只发这一轮的新内容）。
func lastUserText(messages []chatMessage) (string, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		text, err := renderContent(messages[i].Content)
		if err != nil || strings.TrimSpace(text) == "" {
			return "", false
		}
		return text, true
	}
	return "", false
}

// CleanupStale 回收上个进程留下的会话线程（启动时调用一次）。
func (s *Server) CleanupStale(ctx context.Context) {
	for _, c := range s.convos.stale() {
		acc, ok := s.store.Raw(c.AccountID)
		if !ok {
			s.convos.drop(c.Key)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		sess := s.sessionFor(acc)
		_, _ = sess.DeleteJams(cctx, acc.OrgID, []string{c.JamID})
		cancel()
		s.convos.drop(c.Key)
	}
}

// StartConversationGC 定期回收空闲超时的会话线程（默认 30 分钟）。
func (s *Server) StartConversationGC(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.keys.Flush()
				s.store.Flush()
				s.convos.gc(ctx, s.store.Raw, func(ctx context.Context, acc store.Account, jamID string) {
					cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
					defer cancel()
					sess := s.sessionFor(acc)
					_, _ = sess.DeleteJams(cctx, acc.OrgID, []string{jamID})
				})
			}
		}
	}()
}
