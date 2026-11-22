// compat.go 客户端兼容层：WorkBuddy 网关有、而客户端真的会打的那些端点。
//
// 这些不是「装饰」：Claude Code 每次压缩上下文前都会调 /v1/messages/count_tokens，
// 缺了它直接报错；Codex 在 supports_websockets 时会先 GET /v1/responses 探升级，
// 必须给 426 让它立刻、粘性地回落 HTTPS，否则会空转重连。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode"
)

// handleCountTokens 本地估算 Anthropic 形状的输入 token。
//
// 上游没有计数接口，按 workbuddy 的口径估算：CJK 一字≈1 token，其余 4 字符≈1 token，
// base64 图片每张固定 1600（绝不按字符数算，否则一张截图就让客户端误判上下文满）。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	var body any
	if err := decodeJSON(r, &body); err != nil {
		status := http.StatusBadRequest
		if err == errBodyTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		writeAnthropicError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateTokens(body)})
}

func estimateTokens(v any) int {
	switch x := v.(type) {
	case string:
		return textTokens(x)
	case []any:
		n := 0
		for _, it := range x {
			n += estimateTokens(it)
		}
		return n
	case map[string]any:
		n := 0
		for k, it := range x {
			if k == "data" {
				n += 1600
				continue
			}
			n += estimateTokens(it)
		}
		return n
	}
	return 0
}

func textTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
			continue
		}
		other++
	}
	n := cjk + other/4
	if other%4 != 0 {
		n++
	}
	if n == 0 {
		return 1
	}
	return n
}

// handleResponsesWSProbe 回应 Codex 的 WS 升级探测：426 = 立刻回落 HTTPS。
func (s *Server) handleResponsesWSProbe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("upgrade", "websocket")
	w.Header().Set("connection", "Upgrade")
	writeJSON(w, http.StatusUpgradeRequired, map[string]any{
		"error": map[string]any{
			"message": "本网关不支持 /v1/responses 的 WebSocket 传输，请改用 HTTPS POST",
			"type":    "upgrade_required",
		},
	})
}

var errNoDevice = errNoDeviceType{"账号没有绑定设备 id"}

type errNoDeviceType struct{ msg string }

func (e errNoDeviceType) Error() string { return e.msg }

// handleStatus 观测端点：账号、模型、并发与冷却。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	accounts := s.store.List()
	ready, cooling, invalid := 0, 0, 0
	for _, a := range accounts {
		switch {
		case !a.Enabled || a.State == "invalid":
			invalid++
		case a.State == "cooldown":
			cooling++
		default:
			ready++
		}
	}
	modelIDs := []string{}
	if len(accounts) > 0 {
		if acc, ok := s.store.Raw(accounts[0].ID); ok {
			if models, err := s.catalog(r.Context(), acc, false); err == nil {
				for _, m := range models {
					modelIDs = append(modelIDs, m.ID)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": Version,
		"mode":    s.cfg().Capy.Mode,
		"accounts": map[string]any{
			"total": len(accounts), "ready": ready, "cooling": cooling, "invalid": invalid,
		},
		"concurrency": map[string]any{
			"max":       s.cfg().Capy.MaxConcurrent,
			"in_flight": len(s.sem),
		},
		"models":        modelIDs,
		"thread_pool":   s.poolStats(),
		"device_daemon": s.daemonStatus(),
		"thread_mode":   s.cfg().Capy.ThreadMode,
	})
}

// handleCapacity 模型繁忙度（按本地账号状态算，不打上游）。
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	ready := false
	for _, a := range s.store.List() {
		if a.Enabled && a.State != "invalid" && a.State != "cooldown" {
			ready = true
			break
		}
	}
	level := "busy"
	if ready {
		level = "idle"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"busy_level":     level,
		"accounts_ready": ready,
	})
}

// handleChatBusy 兼容官方客户端的「模型忙」预检。
func (s *Server) handleChatBusy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models []string `json:"models"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	ready := false
	for _, a := range s.store.List() {
		if a.Enabled && a.State != "invalid" && a.State != "cooldown" {
			ready = true
			break
		}
	}
	known := map[string]bool{}
	if len(s.store.List()) > 0 {
		if acc, ok := s.store.Raw(s.store.List()[0].ID); ok {
			if models, err := s.catalog(r.Context(), acc, false); err == nil {
				for _, m := range models {
					known[m.ID] = true
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(req.Models))
	for _, m := range req.Models {
		level := "unknown"
		if known[m] || matchModelSuffix(m, known) {
			if ready {
				level = "idle"
			} else {
				level = "busy"
			}
		}
		out = append(out, map[string]any{"model": m, "busy_level": level})
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "OK", "data": out})
}

func matchModelSuffix(name string, known map[string]bool) bool {
	for id := range known {
		if strings.EqualFold(id, name) || strings.HasSuffix(strings.ToLower(id), "/"+strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// usageAliases 把缓存字段补齐成兼容别名（OpenAI / Anthropic 客户端都认）。
func usageWithCacheAliases(u map[string]any) map[string]any {
	for _, key := range []string{"cached_tokens", "cache_read_input_tokens"} {
		if v, ok := u[key]; ok {
			u[key] = v
		}
	}
	return u
}

// anthropicUsage 生成 Anthropic 形状的 usage（含 cache 读/写）。
func anthropicUsage(prompt, completion, cacheRead, cacheWrite int) map[string]any {
	return map[string]any{
		"input_tokens":                prompt,
		"output_tokens":               completion,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation_input_tokens": cacheWrite,
	}
}
