// Package store 管理 Capy 账号池与请求日志（内存环形缓冲）。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Account 是一个 Capy 账号（一份 Clerk 会话 + 绑定的设备）。
type Account struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Email         string    `json:"email,omitempty"`
	UserID        string    `json:"user_id,omitempty"`
	Cookie        string    `json:"cookie"`
	SessionID     string    `json:"session_id,omitempty"`
	OrgID         string    `json:"org_id,omitempty"`
	OrgName       string    `json:"org_name,omitempty"`
	DeviceID      string    `json:"device_id,omitempty"`
	DeviceToken   string    `json:"device_token,omitempty"`
	Mode          string    `json:"mode,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
	WorkspaceMode string    `json:"workspace_mode,omitempty"`
	WorkspaceRoot string    `json:"workspace_root,omitempty"`
	Enabled       bool      `json:"enabled"`
	AddedAt       time.Time `json:"added_at"`
	LastUsedAt    time.Time `json:"last_used_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	State         string    `json:"state,omitempty"` // ok / invalid / cooldown / unknown
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	Source        string    `json:"source,omitempty"`
}

// View 是给面板展示的账号视图（隐藏 cookie 明文）。
type View struct {
	Account
	CookieSet bool `json:"cookie_set"`
}

// Redact 生成面板视图。
func (a Account) Redact() View {
	v := View{Account: a, CookieSet: a.Cookie != ""}
	v.Cookie = ""
	if n := len(a.Cookie); n > 12 {
		v.Cookie = a.Cookie[:6] + "…" + a.Cookie[n-4:]
	}
	if n := len(a.SessionID); n > 10 {
		v.SessionID = a.SessionID[:6] + "…" + a.SessionID[n-4:]
	}
	return v
}

// Store 是账号池。
type Store struct {
	mu       sync.RWMutex
	path     string
	accounts []Account
	rr       int
	dirty    bool
}

// Open 载入账号池；文件不存在时创建空池。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "accounts.json")}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var payload struct {
		Accounts []Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", s.path, err)
	}
	s.accounts = payload.Accounts
	for i := range s.accounts {
		if s.accounts[i].State == "" {
			s.accounts[i].State = "unknown"
		}
	}
	return s, nil
}

// Path 返回账号池文件路径。
func (s *Store) Path() string { return s.path }

func (s *Store) saveLocked() error {
	payload := struct {
		Accounts []Account `json:"accounts"`
	}{Accounts: s.accounts}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List 返回全部账号视图。
func (s *Store) List() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]View, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a.Redact())
	}
	return out
}

// Raw 返回账号副本（含 cookie，仅供服务内部使用）。
func (s *Store) Raw(id string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// Count 返回账号数量。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts)
}

// Add 追加账号；同 cookie 的账号会被替换。
func (s *Store) Add(a Account) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(a.Cookie) == "" {
		return Account{}, errors.New("cookie 不能为空")
	}
	if a.ID == "" {
		a.ID = fmt.Sprintf("acc_%d", time.Now().UnixNano())
	}
	if a.Name == "" {
		if a.Email != "" {
			a.Name = a.Email
		} else {
			a.Name = a.ID
		}
	}
	if a.AddedAt.IsZero() {
		a.AddedAt = time.Now()
	}
	if a.State == "" {
		a.State = "unknown"
	}
	a.Enabled = true
	for i, old := range s.accounts {
		if old.Cookie == a.Cookie || (a.Email != "" && old.Email == a.Email) {
			a.ID = old.ID
			a.AddedAt = old.AddedAt
			s.accounts[i] = a
			return a, s.saveLocked()
		}
	}
	s.accounts = append(s.accounts, a)
	return a, s.saveLocked()
}

// Update 修改账号。
func (s *Store) Update(id string, fn func(*Account)) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			fn(&s.accounts[i])
			if err := s.saveLocked(); err != nil {
				return Account{}, err
			}
			return s.accounts[i], nil
		}
	}
	return Account{}, errors.New("账号不存在")
}

// Delete 删除账号。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			return s.saveLocked()
		}
	}
	return errors.New("账号不存在")
}

// Next 轮转取一个可用账号。
//
// 冷却中的账号会被跳过（上游 5xx/429 后的短暂退避）；如果全都在冷却，
// 取最早解除的那个——宁可带着退避去试，也不要直接让客户端失败。
func (s *Store) Next() (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.accounts)
	if n == 0 {
		return Account{}, errors.New("还没有导入 Capy 账号")
	}
	now := time.Now()
	var cooling []int
	for i := 0; i < n; i++ {
		idx := (s.rr + i) % n
		a := s.accounts[idx]
		if !a.Enabled || a.State == "invalid" {
			continue
		}
		if !a.CooldownUntil.IsZero() && now.Before(a.CooldownUntil) {
			cooling = append(cooling, idx)
			continue
		}
		return s.pickLocked(idx), nil
	}
	if len(cooling) > 0 {
		best := cooling[0]
		for _, idx := range cooling[1:] {
			if s.accounts[idx].CooldownUntil.Before(s.accounts[best].CooldownUntil) {
				best = idx
			}
		}
		return s.pickLocked(best), nil
	}
	return Account{}, errors.New("没有可用账号（全部被禁用或失效）")
}

func (s *Store) pickLocked(idx int) Account {
	s.accounts[idx].LastUsedAt = time.Now()
	if !s.accounts[idx].CooldownUntil.IsZero() && time.Now().After(s.accounts[idx].CooldownUntil) {
		s.accounts[idx].CooldownUntil = time.Time{}
		if s.accounts[idx].State == "cooldown" {
			s.accounts[idx].State = "ok"
		}
	}
	a := s.accounts[idx]
	s.rr = (idx + 1) % len(s.accounts)
	s.dirty = true // 每次选号都写盘会拖垮公开 API：交给后台 Flush
	return a
}

// Flush 把 lastUsed/dirty 状态落盘（后台定时调用）。
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	if err := s.saveLocked(); err == nil {
		s.dirty = false
	}
}

// MarkCooldown 给账号记一段冷却（上游 5xx / 429 / 网络故障后调用）。
func (s *Store) MarkCooldown(id string, d time.Duration, errMsg string) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID != id {
			continue
		}
		until := time.Now().Add(d)
		if until.After(s.accounts[i].CooldownUntil) {
			s.accounts[i].CooldownUntil = until
		}
		s.accounts[i].State = "cooldown"
		s.accounts[i].LastError = errMsg
		_ = s.saveLocked()
		return
	}
}

// MarkState 记录账号状态。
func (s *Store) MarkState(id, state, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID != id {
			continue
		}
		s.accounts[i].State = state
		s.accounts[i].LastError = errMsg
		_ = s.saveLocked()
		return
	}
}

// ---------------------------------------------------------------- request log

// LogEntry 是一条请求日志。
type LogEntry struct {
	Time         string `json:"time"`
	Account      string `json:"account"`
	Model        string `json:"model"`
	JamID        string `json:"jam_id"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	PromptChars  int    `json:"prompt_chars"`
	PromptTokens int    `json:"prompt_tokens"`
	OutputTokens int    `json:"output_tokens"`
	CacheTokens  int    `json:"cache_tokens"`
	DurationMS   int64  `json:"duration_ms"`
	Stream       bool   `json:"stream"`
	Tools        int    `json:"tools"`
	AdBrand      string `json:"ad_brand,omitempty"`
	Client       string `json:"client,omitempty"`
	Key          string `json:"key,omitempty"`
}

// Log 是定长环形日志。
type Log struct {
	mu      sync.RWMutex
	entries []LogEntry
	max     int
}

// NewLog 构造日志环。
func NewLog(max int) *Log {
	if max <= 0 {
		max = 200
	}
	return &Log{max: max}
}

// Push 追加一条日志。
func (l *Log) Push(e LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
}

// Recent 返回最近 n 条（新在前）。
func (l *Log) Recent(n int) []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > len(l.entries) {
		n = len(l.entries)
	}
	out := make([]LogEntry, 0, n)
	for i := len(l.entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

// Totals 汇总 token 用量（缓存命中用 cacheReadTokens）。
func (l *Log) Totals() (requests, promptTokens, outputTokens, cacheTokens int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	requests = len(l.entries)
	for _, e := range l.entries {
		promptTokens += e.PromptTokens
		outputTokens += e.OutputTokens
		cacheTokens += e.CacheTokens
	}
	return
}

// Stats 汇总日志统计。
func (l *Log) Stats() (total, ok, failed int, avgMS int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	total = len(l.entries)
	var sum int64
	for _, e := range l.entries {
		if e.Status == "ok" {
			ok++
		} else {
			failed++
		}
		sum += e.DurationMS
	}
	if total > 0 {
		avgMS = sum / int64(total)
	}
	return
}
