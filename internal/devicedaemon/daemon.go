// Package devicedaemon 用设备凭据直连 Capy 的设备隧道，自己扮演「设备」执行云端下发的 invoke，
// 从而在没有 Capy 桌面端、没有云额度的情况下跑完整回合。
package devicedaemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const tunnelURL = "wss://device-tunnel-production.cloud.capy.ai/tunnel"

// Config 是伪造设备需要的参数。
type Config struct {
	Token         string // capyd_...
	DeviceID      string // device_...
	Workspace     string // 链接给云端的工作目录（会在 hello.workspaces 里声明）
	Hostname      string
	ClientVersion string
	// Policy: "text"=只回文本，拒绝一切文件/命令（默认，适合对外 API）；
	//         "readonly"=允许只读白名单命令；"all"=允许任意命令（仅自己机器用）。
	Policy         string
	BashAllowAll   bool     // 兼容旧字段：true 等价 Policy=all
	BashAllowlist  []string // 自定义白名单前缀
	DenySubstrings []string // 命中即拒绝（默认屏蔽长期记忆路径）
}

// Daemon 是常驻的设备连接。
type Daemon struct {
	cfg Config

	mu        sync.Mutex
	online    bool
	lastErr   string
	sessions  int
	memory    []string // 云端尝试读取的记忆文件（被策略拦下的）
	resultsMu sync.Mutex
	results   map[string]any // invocationId -> 结果（云端会 GET /v1/invocations/{id} 取回）

	cancel context.CancelFunc
}

func (d *Daemon) Status() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	mem := make([]string, len(d.memory))
	copy(mem, d.memory)
	return map[string]any{"online": d.online, "last_error": d.lastErr, "sessions": d.sessions, "memory_attempts": mem}
}

// Start 启动后台连接循环。
func Start(ctx context.Context, cfg Config) *Daemon {
	if cfg.Workspace == "" {
		home, _ := os.UserHomeDir()
		cfg.Workspace = filepath.Join(home, "capy2api-workspace")
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "capy2api-device"
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = "desktop 0.9.2"
	}
	if len(cfg.DenySubstrings) == 0 {
		cfg.DenySubstrings = []string{".capy/drive", "AGENTS.md", "CLAUDE.md"}
	}
	if cfg.Policy == "" {
		cfg.Policy = "text"
	}
	if cfg.BashAllowAll {
		cfg.Policy = "all"
	}
	if cfg.Policy != "text" && cfg.Policy != "readonly" && cfg.Policy != "all" {
		cfg.Policy = "text"
	}
	if len(cfg.BashAllowlist) == 0 {
		cfg.BashAllowlist = []string{"ls", "pwd", "cat", "head", "tail", "wc", "git", "find", "grep", "rg", "sed", "echo", "true", "which", "command", "stat", "file", "tree", "du", "df", "date", "env", "printenv", "python3 --version", "node --version"}
	}
	_ = os.MkdirAll(cfg.Workspace, 0o755)
	d := &Daemon{cfg: cfg, results: map[string]any{}}
	cctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	go d.loop(cctx)
	return d
}

// Stop 停止连接。
func (d *Daemon) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Daemon) loop(ctx context.Context) {
	for ctx.Err() == nil {
		err := d.runOnce(ctx)
		d.mu.Lock()
		d.online = false
		d.lastErr = err.Error()
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

type ctrl struct {
	Type      string         `json:"type"`
	Method    string         `json:"method"`
	StreamID  uint32         `json:"streamId"`
	Kind      string         `json:"kind"`
	Path      string         `json:"path"`
	Headers   map[string]any `json:"headers"`
	BackingID string         `json:"backingId"`
}

type stream struct {
	path   string
	method string
	body   []byte
}

func (d *Daemon) runOnce(ctx context.Context) error {
	cfgWS, err := websocket.NewConfig(tunnelURL, "https://capy.ai")
	if err != nil {
		return err
	}
	cfgWS.Header.Set("authorization", "Bearer "+d.cfg.Token)
	conn, err := websocket.DialConfig(cfgWS)
	if err != nil {
		return err
	}
	defer conn.Close()

	sendJSON := func(v any) error {
		raw, _ := json.Marshal(v)
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return websocket.Message.Send(conn, string(raw))
	}
	sendBytes := func(b []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return websocket.Message.Send(conn, b)
	}

	workspaces := []any{map[string]any{
		"root": d.cfg.Workspace, "repos": []any{}, "branch": "", "dirty": false,
		"insertions": 0, "deletions": 0, "missing": false,
	}}
	if err := sendJSON(map[string]any{
		"type": "hello", "contractVersion": 2, "deviceId": d.cfg.DeviceID,
		"hostname": d.cfg.Hostname, "os": "darwin", "workspaces": workspaces,
		"worktreeBase": d.cfg.Workspace, "clientVersion": d.cfg.ClientVersion, "drive": false,
	}); err != nil {
		return err
	}

	streams := map[uint32]*stream{}
	for ctx.Err() == nil {
		var raw []byte
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Second))
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			return err
		}
		var c ctrl
		if json.Unmarshal(raw, &c) == nil && c.Type != "" {
			switch c.Type {
			case "ping":
				_ = sendJSON(map[string]any{"type": "pong"})
			case "session-config":
				d.mu.Lock()
				d.online = true
				d.sessions++
				d.lastErr = ""
				d.mu.Unlock()
				log.Printf("devicedaemon: session ready (backing %s)", c.BackingID)
				_ = sendJSON(map[string]any{"type": "workspaces-changed", "workspaces": workspaces})
				_ = sendJSON(map[string]any{"type": "ready"})
			case "open":
				streams[c.StreamID] = &stream{path: c.Path, method: c.Method}
			case "close":
				st := streams[c.StreamID]
				if st == nil {
					continue
				}
				delete(streams, c.StreamID)
				status, body := d.respond(st, c)
				rawBody, _ := json.Marshal(body)
				_ = sendJSON(map[string]any{"type": "response", "streamId": c.StreamID, "status": status,
					"headers": map[string]string{"content-type": "application/json"}})
				frame := make([]byte, 4+len(rawBody))
				binary.BigEndian.PutUint32(frame, c.StreamID)
				copy(frame[4:], rawBody)
				_ = sendBytes(frame)
				_ = sendJSON(map[string]any{"type": "close", "streamId": c.StreamID})
			}
			continue
		}
		if len(raw) >= 4 {
			id := binary.BigEndian.Uint32(raw[:4])
			if st := streams[id]; st != nil {
				st.body = append(st.body, raw[4:]...)
			}
		}
	}
	return ctx.Err()
}

// respond 计算调用结果，并按 invocationId 缓存，供云端 GET 取回。
func (d *Daemon) respond(st *stream, c ctrl) (int, any) {
	status, body := d.respondInner(st, c)
	// 设备网关的成功响应是 {status:"succeeded", result:...} 信封；GET 轮询取同一个信封。
	envelope := map[string]any{"status": "succeeded", "result": body}
	if st.method == "POST" && strings.HasPrefix(st.path, "/v1/invocations") {
		var req struct {
			InvocationID string `json:"invocationId"`
		}
		if json.Unmarshal(st.body, &req) == nil && req.InvocationID != "" {
			d.resultsMu.Lock()
			if d.results == nil {
				d.results = map[string]any{}
			}
			d.results[req.InvocationID] = envelope
			if len(d.results) > 512 {
				for k := range d.results {
					delete(d.results, k)
					if len(d.results) <= 256 {
						break
					}
				}
			}
			d.resultsMu.Unlock()
		}
	}
	if st.method == "POST" && strings.HasPrefix(st.path, "/v1/invocations") {
		return status, envelope
	}
	return status, body
}

func (d *Daemon) respondInner(st *stream, c ctrl) (int, any) {
	var req struct {
		InvocationID string         `json:"invocationId"`
		Handler      string         `json:"handler"`
		Params       map[string]any `json:"params"`
	}
	_ = json.Unmarshal(st.body, &req)
	body := string(st.body)
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	log.Printf("devicedaemon: invoke method=%s path=%s handler=%q params=%v body=%q", st.method, st.path, req.Handler, truncateParams(req.Params), body)

	// 云端用 GET /v1/invocations/{id} 长轮询取回某次调用的结果。
	if strings.HasPrefix(st.path, "/v1/invocations/") && st.method != "POST" {
		id := strings.TrimPrefix(st.path, "/v1/invocations/")
		if i := strings.Index(id, "?"); i >= 0 {
			id = id[:i]
		}
		d.resultsMu.Lock()
		res, ok := d.results[id]
		d.resultsMu.Unlock()
		if ok {
			return 200, res
		}
		return 404, map[string]any{"error": "invocation not found"}
	}

	switch req.Handler {
	case "context_scan":
		// 返回空上下文：既不注入 AGENTS.md 记忆，也不扫描 skills。
		return 200, map[string]any{"kind": "context_scan", "instructions": []any{}, "skills": []any{}, "nested": []any{}, "rules": []any{}}
	case "volume_landed":
		return 200, map[string]any{"kind": "volume_landed", "mounts": []any{}, "landing": false, "checkpoints": false, "directories": []any{}, "unretained": []any{}, "handback": false}
	case "volume_probe":
		return 200, map[string]any{"kind": "volume_probe", "mounts": []any{}}
	case "volume_trees":
		return 200, map[string]any{"kind": "volume_trees", "trees": []any{}}
	case "volume_reconcile":
		return 200, map[string]any{"kind": "volume_reconcile"}
	case "write", "edit", "apply_patch":
		return 200, map[string]any{"kind": "failure", "isError": true,
			"message": "平台自带写文件工具不可用。请改用客户端声明的 write/edit 工具完成文件修改，不要重试平台工具。"}
	case "read":
		path, _ := req.Params["path"].(string)
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(d.cfg.Workspace, path)
		}
		d.noteMemory(path)
		if !d.pathAllowed(path) {
			return 200, map[string]any{"kind": "failure", "isError": true,
				"message": "平台自带读文件工具不可用（路径受限）。请改用客户端声明的 read 工具，不要重试平台工具。"}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return 200, map[string]any{"kind": "failure", "message": "File not found: " + path, "isError": true}
		}
		text := string(data)
		return 200, map[string]any{"kind": "read", "content": text, "offset": 1, "totalLines": strings.Count(text, "\n") + 1, "isError": false}
	case "bash":
		return 200, d.runBash(req.Params)
	}
	if strings.HasPrefix(st.path, "/v1/git/capture") {
		return 200, map[string]any{"repos": []any{}, "baseRefs": []any{}}
	}
	return 200, map[string]any{"kind": "failure", "message": "unsupported handler: " + req.Handler, "isError": true}
}

func (d *Daemon) noteMemory(path string) {
	if !strings.Contains(path, "AGENTS.md") && !strings.Contains(path, "CLAUDE.md") && !strings.Contains(path, ".capy/drive") {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.memory {
		if p == path {
			return
		}
	}
	d.memory = append(d.memory, path)
	if len(d.memory) > 20 {
		d.memory = d.memory[len(d.memory)-20:]
	}
}

func (d *Daemon) pathAllowed(path string) bool {
	if d.cfg.Policy == "text" {
		return false
	}
	clean := filepath.Clean(path)
	base := filepath.Clean(d.cfg.Workspace)
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return false
	}
	for _, bad := range d.cfg.DenySubstrings {
		if strings.Contains(clean, bad) {
			return false
		}
	}
	return true
}

func (d *Daemon) runBash(params map[string]any) map[string]any {
	// 纯文本模式：对外 API 不允许云端 agent 在本机执行任何命令。
	if d.cfg.Policy == "text" {
		return map[string]any{"kind": "failure", "message": "this device is text-only: shell access is disabled by the gateway", "isError": true}
	}
	cmdStr, _ := params["command"].(string)
	if strings.TrimSpace(cmdStr) == "" {
		return map[string]any{"kind": "failure", "message": "empty command", "isError": true}
	}
	for _, bad := range d.cfg.DenySubstrings {
		if strings.Contains(cmdStr, bad) {
			d.noteMemory(cmdStr)
			return map[string]any{"kind": "failure", "message": "command denied by policy", "isError": true}
		}
	}
	if d.cfg.Policy == "readonly" && strings.ContainsAny(cmdStr, ";|&$`><\n") {
		return map[string]any{"kind": "failure", "message": "shell metacharacters are not allowed in readonly mode", "isError": true}
	}
	if d.cfg.Policy == "readonly" && !allowedCommand(cmdStr, d.cfg.BashAllowlist) {
		return map[string]any{"kind": "failure", "message": "command not in allowlist: " + firstWord(cmdStr), "isError": true}
	}
	timeout := 30 * time.Second
	if v, ok := params["timeoutSeconds"].(float64); ok && v > 0 && v < 120 {
		timeout = time.Duration(v) * time.Second
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-lc", cmdStr)
	cmd.Dir = d.cfg.Workspace
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return map[string]any{"kind": "bash", "stdout": out.String(), "stderr": errb.String(),
		"exitCode": code, "durationMs": time.Since(start).Milliseconds(),
		"timedOut": ctx.Err() != nil, "isError": code != 0}
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func allowedCommand(cmd string, allow []string) bool {
	word := firstWord(cmd)
	for _, a := range allow {
		if word == a || strings.HasPrefix(cmd, a) {
			return true
		}
	}
	return false
}

var _ = fmt.Sprintf

func truncateParams(p map[string]any) string {
	out := map[string]any{}
	for k, v := range p {
		if str, ok := v.(string); ok && len(str) > 80 {
			out[k] = str[:80] + "…"
			continue
		}
		out[k] = v
	}
	raw, _ := json.Marshal(out)
	if len(raw) > 240 {
		return string(raw[:240]) + "…"
	}
	return string(raw)
}
