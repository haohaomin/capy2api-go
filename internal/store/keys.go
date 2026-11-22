// keys.go 订阅制 API Key：每个 key 有自己的配额、速率、模型白名单与用量。
//
// 公开对外时不能只有「一个 api_key」：那既没法按用户计量，也没法隔离会话与限流。
// 这里给出最薄的订阅层：
//   - 主密钥（config.api_key）永远可用，等同管理员，不受配额限制；
//   - 子 key 记请求数 / token 用量 / RPM 限额 / 模型白名单；
//   - 计数器在内存里累计，定期落盘（进程退出再存一次）。
package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// APIKey 是一条订阅密钥。
type APIKey struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Key              string    `json:"key"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	Requests         int64     `json:"requests"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	QuotaRequests    int64     `json:"quota_requests,omitempty"` // 0 = 不限
	QuotaTokens      int64     `json:"quota_tokens,omitempty"`   // 0 = 不限（prompt+completion）
	RPMLimit         int       `json:"rpm_limit,omitempty"`      // 0 = 不限
	MaxConcurrent    int       `json:"max_concurrent,omitempty"` // 0 = 用全局
	AllowedModels    []string  `json:"allowed_models,omitempty"` // 空 = 全部
	AccountID        string    `json:"account_id,omitempty"`     // 绑定哪个 Capy 账号（空 = 共享池）
	LastUsedAt       time.Time `json:"last_used_at,omitempty"`
	Note             string    `json:"note,omitempty"`
}

// View 是面板视图（隐藏完整 key）。
type APIKeyView struct {
	APIKey
	KeyHint string `json:"key_hint"`
}

// Redact 生成面板视图。
func (k APIKey) Redact() APIKeyView {
	v := APIKeyView{APIKey: k}
	if len(k.Key) > 12 {
		v.KeyHint = k.Key[:8] + "…" + k.Key[len(k.Key)-4:]
	} else {
		v.KeyHint = k.Key
	}
	v.Key = ""
	return v
}

// KeyStore 是密钥表。
type KeyStore struct {
	mu    sync.RWMutex
	path  string
	keys  []APIKey
	dirty bool
	rpm   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// OpenKeys 载入密钥表。
func OpenKeys(dir string) (*KeyStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &KeyStore{path: filepath.Join(dir, "keys.json"), rpm: map[string]*bucket{}}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var payload struct {
		Keys []APIKey `json:"keys"`
	}
	if err := jsonUnmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析 keys.json 失败: %w", err)
	}
	s.keys = payload.Keys
	return s, nil
}

// Path 返回密钥文件路径。
func (s *KeyStore) Path() string { return s.path }

// List 返回视图列表。
func (s *KeyStore) List() []APIKeyView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]APIKeyView, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k.Redact())
	}
	return out
}

// Lookup 按明文 key 查找（同时检查启用与过期）。
func (s *KeyStore) Lookup(key string) (APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if subtleEqual(k.Key, key) {
			if !k.Enabled {
				return k, false
			}
			if !k.ExpiresAt.IsZero() && time.Now().After(k.ExpiresAt) {
				return k, false
			}
			return k, true
		}
	}
	return APIKey{}, false
}

// Create 生成一条新 key。
func (s *KeyStore) Create(k APIKey) (APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k.ID == "" {
		k.ID = "key_" + randomToken(6)
	}
	if k.Key == "" {
		k.Key = "sk-capy-" + randomToken(24)
	}
	if k.Name == "" {
		k.Name = k.ID
	}
	k.Enabled = true
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now()
	}
	s.keys = append(s.keys, k)
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		return APIKey{}, err
	}
	return k, nil
}

// Update 修改 key。
func (s *KeyStore) Update(id string, fn func(*APIKey)) (APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID == id {
			fn(&s.keys[i])
			s.dirty = true
			if err := s.saveLocked(); err != nil {
				return APIKey{}, err
			}
			return s.keys[i], nil
		}
	}
	return APIKey{}, errors.New("key 不存在")
}

// Delete 删除 key。
func (s *KeyStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID == id {
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			delete(s.rpm, id)
			s.dirty = true
			return s.saveLocked()
		}
	}
	return errors.New("key 不存在")
}

// AllowRPM 令牌桶限速：每分钟 rpm 次，返回是否放行。
func (s *KeyStore) AllowRPM(k APIKey) bool {
	if k.RPMLimit <= 0 {
		return true
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.rpm[k.ID]
	if b == nil {
		b = &bucket{tokens: float64(k.RPMLimit), last: now}
		s.rpm[k.ID] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * float64(k.RPMLimit) / 60.0
	if b.tokens > float64(k.RPMLimit) {
		b.tokens = float64(k.RPMLimit)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// QuotaExceeded 检查请求数 / token 配额。
func (k APIKey) QuotaExceeded() string {
	if k.QuotaRequests > 0 && k.Requests >= k.QuotaRequests {
		return "请求数配额已用完"
	}
	if k.QuotaTokens > 0 && k.PromptTokens+k.CompletionTokens >= k.QuotaTokens {
		return "token 配额已用完"
	}
	return ""
}

// Record 记录一次成功请求的用量（内存累计，定期落盘）。
func (s *KeyStore) Record(id string, promptTokens, completionTokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != id {
			continue
		}
		s.keys[i].Requests++
		s.keys[i].PromptTokens += int64(promptTokens)
		s.keys[i].CompletionTokens += int64(completionTokens)
		s.keys[i].LastUsedAt = time.Now()
		s.dirty = true
		return
	}
}

// Flush 把内存里的计数落盘（后台定时调用）。
func (s *KeyStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	if err := s.saveLocked(); err == nil {
		s.dirty = false
	}
}

func (s *KeyStore) saveLocked() error {
	payload := struct {
		Keys []APIKey `json:"keys"`
	}{Keys: s.keys}
	raw, err := jsonMarshal(payload)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("x", n)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(raw []byte, dst any) error { return json.Unmarshal(raw, dst) }
