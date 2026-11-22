// Package capy 是 Capy Web 应用的客户端：Clerk 会话换 JWT，再调用 /app/* 路由。
//
// 鉴权链：Capy 桌面端把 __client（Clerk client JWT）存进 session-credential.json，
// 用 Cookie: __client=<jwt> 向 clerk.capy.ai 换 60 秒有效的 session JWT，
// 之后所有 /app/* 请求带 Authorization: Bearer <jwt> 与 x-capy-org。
package capy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Client 保存上游协议常量。
type Client struct {
	HTTP      *http.Client
	APIBase   string
	ClerkBase string
	JSVersion string
	Features  string
	Surface   string
	Timezone  string
	ClientID  string
	UserAgent string
}

// NewClient 构造客户端；超时按单次请求计，回合总超时由调用方控制。
func NewClient(apiBase, clerkBase, jsVersion, features, surface, timezone, clientID string) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
		APIBase:   strings.TrimRight(apiBase, "/"),
		ClerkBase: strings.TrimRight(clerkBase, "/"),
		JSVersion: jsVersion,
		Features:  features,
		Surface:   surface,
		Timezone:  timezone,
		ClientID:  clientID,
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36",
	}
}

// APIError 是上游返回的结构化错误。
type APIError struct {
	Status  int
	Tag     string
	Message string
	Body    string
}

func (e *APIError) Error() string {
	switch {
	case e.Tag != "" && e.Message != "":
		return fmt.Sprintf("capy %s: %s (HTTP %d)", e.Tag, e.Message, e.Status)
	case e.Message != "":
		return fmt.Sprintf("capy: %s (HTTP %d)", e.Message, e.Status)
	case e.Body != "":
		return fmt.Sprintf("capy HTTP %d: %s", e.Status, truncate(e.Body, 200))
	default:
		return fmt.Sprintf("capy HTTP %d", e.Status)
	}
}

// StatusOf 返回错误里的上游状态码（非 APIError 返回 0）。
func StatusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// Session 是一个账号的凭据与 JWT 缓存。
type Session struct {
	Client    *Client
	Cookie    string // __client cookie 值（Clerk client JWT）
	SessionID string // sess_xxx，可空（自动发现）

	mu      sync.Mutex
	token   string
	exp     time.Time
	adCache map[string]*Ad
}

// Token 返回有效 JWT；过期前自动换新。
func (s *Session) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Until(s.exp) > 20*time.Second {
		return s.token, nil
	}
	if strings.TrimSpace(s.Cookie) == "" {
		return "", &APIError{Status: 401, Message: "账号缺少 __client cookie，请重新导入"}
	}
	if s.SessionID == "" {
		sid, err := s.discoverSession(ctx)
		if err != nil {
			return "", err
		}
		s.SessionID = sid
	}
	u := fmt.Sprintf("%s/v1/client/sessions/%s/tokens?_clerk_js_version=%s",
		s.Client.ClerkBase, url.PathEscape(s.SessionID), url.QueryEscape(s.Client.JSVersion))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("cookie", "__client="+s.Cookie)
	req.Header.Set("origin", "https://capy.ai")
	req.Header.Set("user-agent", s.Client.UserAgent)
	req.Header.Set("accept", "application/json")
	resp, err := s.Client.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("clerk 换票失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", &APIError{Status: resp.StatusCode, Message: "Clerk 会话不可用（cookie 过期或已登出）", Body: string(raw)}
	}
	var out struct {
		JWT string `json:"jwt"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.JWT == "" {
		return "", &APIError{Status: 502, Message: "Clerk 未返回 jwt", Body: string(raw)}
	}
	s.token = out.JWT
	if exp := jwtExp(out.JWT); !exp.IsZero() {
		s.exp = exp
	} else {
		s.exp = time.Now().Add(50 * time.Second)
	}
	return s.token, nil
}

// Invalidate 丢弃缓存的 JWT（上游 401 时调用）。
func (s *Session) Invalidate() {
	s.mu.Lock()
	s.token = ""
	s.exp = time.Time{}
	s.mu.Unlock()
}

func (s *Session) discoverSession(ctx context.Context) (string, error) {
	u := fmt.Sprintf("%s/v1/client?_clerk_api_version=2026-08-20&_clerk_js_version=%s",
		s.Client.ClerkBase, url.QueryEscape(s.Client.JSVersion))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("cookie", "__client="+s.Cookie)
	req.Header.Set("origin", "https://capy.ai")
	req.Header.Set("user-agent", s.Client.UserAgent)
	resp, err := s.Client.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("查询 Clerk 会话失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &APIError{Status: resp.StatusCode, Message: "查询 Clerk 会话失败", Body: string(raw)}
	}
	var out struct {
		Response struct {
			Sessions []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"sessions"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &APIError{Status: 502, Message: "解析 Clerk 会话失败", Body: string(raw)}
	}
	for _, sess := range out.Response.Sessions {
		if sess.Status == "active" {
			return sess.ID, nil
		}
	}
	return "", &APIError{Status: 401, Message: "Cookie 中没有活跃的 Clerk 会话，请在 Capy 重新登录"}
}

// Do 发起一次 /app/* 请求。org 非空时带上 x-capy-org（Org 路由必需）。
//
// 只对 GET 做重试：创建线程/发消息不是幂等操作，重放会留下孤儿线程。
func (s *Session) Do(ctx context.Context, method, path, org string, body any, out any) error {
	return s.do(ctx, method, path, org, body, out, true)
}

func (s *Session) do(ctx context.Context, method, path, org string, body any, out any, retryAuth bool) error {
	if !strings.HasPrefix(path, "/app/") {
		return fmt.Errorf("只允许 /app/ 路径，收到 %q", path)
	}
	token, err := s.Token(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.Client.APIBase+path, reader)
	if err != nil {
		return err
	}
	// CAPY2API_DUMP=1 时把出站请求体打到日志（脱敏：不打印 authorization/cookie）。
	if os.Getenv("CAPY2API_DUMP") == "1" {
		if bodyBytes, berr := json.Marshal(body); berr == nil {
			out := string(bodyBytes)
			if len(out) > 4000 {
				out = out[:4000] + "…(truncated)"
			}
			log.Printf("DUMP %s %s body=%s", method, path, out)
		}
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("origin", "https://capy.ai")
	req.Header.Set("referer", "https://capy.ai/")
	req.Header.Set("user-agent", s.Client.UserAgent)
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if org != "" {
		req.Header.Set("x-capy-org", org)
	}
	if s.Client.Features != "" {
		req.Header.Set("x-capy-client-features", s.Client.Features)
	}
	if s.Client.Surface != "" {
		req.Header.Set("x-capy-client-surface", s.Client.Surface)
	}
	if s.Client.Timezone != "" {
		req.Header.Set("x-capy-client-timezone", s.Client.Timezone)
	}
	if s.Client.ClientID != "" {
		req.Header.Set("x-capy-client-id", s.Client.ClientID)
	}
	resp, err := s.Client.HTTP.Do(req)
	if err != nil {
		if isIdempotent(method) && retryAuth {
			// 网络抖动：幂等请求退避重试一次。
			if werr := sleepCtx(ctx, 300*time.Millisecond); werr == nil {
				return s.do(ctx, method, path, org, body, out, false)
			}
		}
		return fmt.Errorf("上游请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized && retryAuth {
		// JWT 可能刚好过期：换票重试一次。
		s.Invalidate()
		return s.do(ctx, method, path, org, body, out, false)
	}
	if resp.StatusCode >= 500 && isIdempotent(method) && retryAuth {
		if werr := sleepCtx(ctx, 500*time.Millisecond); werr == nil {
			return s.do(ctx, method, path, org, body, out, false)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode, Body: string(raw)}
		var parsed struct {
			Tag     string `json:"_tag"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil {
			ae.Tag, ae.Message = parsed.Tag, parsed.Message
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &APIError{Status: resp.StatusCode, Message: "上游响应不是预期 JSON", Body: string(raw)}
	}
	return nil
}

func jwtExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	p := parts[1]
	if pad := len(p) % 4; pad > 0 {
		p += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
