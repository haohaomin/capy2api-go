// Package browserlogin 用本机 Chrome + CDP 完成「浏览器登录并自动取回 __client」。
//
// 不依赖 Capy 桌面端：打开 https://capy.ai/sign-in，用户在浏览器里正常登录（Google/邮箱/验证码都行），
// 我们从 Chrome 的 cookie 罐里读出 Clerk 的 __client，交给网关换 JWT。
package browserlogin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

// SignInURL 是 Capy 网页端的登录入口（Clerk 托管页，登录后回 capy.ai）。
const SignInURL = "https://capy.ai/sign-in"

// Flow 是一次浏览器登录会话。
type Flow struct {
	Port    int
	Profile string
	cmd     *exec.Cmd
}

// ChromePath 返回本机 Chrome 可执行文件路径。
func ChromePath() (string, error) {
	candidates := []string{}
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"} {
			if p, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, p)
			}
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("没有找到 Chrome/Edge，请安装后重试，或手动粘贴 __client cookie")
}

// Start 启动一个独立的 Chrome 实例（独立 profile，不影响你日常浏览器），打开登录页。
func Start(ctx context.Context) (*Flow, error) {
	chrome, err := ChromePath()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "capy2api-login-")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(chrome,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--new-window",
		SignInURL,
	)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	return &Flow{Port: port, Profile: profile, cmd: cmd}, nil
}

// Close 关闭浏览器并清理临时 profile。
func (f *Flow) Close() {
	if f == nil {
		return
	}
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
		_, _ = f.cmd.Process.Wait()
	}
	_ = os.RemoveAll(f.Profile)
}

// WaitCookie 轮询 Chrome 的 cookie 罐，直到出现 __client 或超时。
func (f *Flow) WaitCookie(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
		cookies, err := f.cookies(ctx)
		if err != nil {
			continue
		}
		for _, c := range cookies {
			if c.Name == "__client" && strings.TrimSpace(c.Value) != "" {
				// 给 Clerk 一点时间把会话写完整（登录成功后 cookie 会再轮换一次）。
				time.Sleep(1200 * time.Millisecond)
				if again, err := f.cookies(ctx); err == nil {
					for _, c2 := range again {
						if c2.Name == "__client" && strings.TrimSpace(c2.Value) != "" {
							return c2.Value, nil
						}
					}
				}
				return c.Value, nil
			}
		}
	}
	return "", fmt.Errorf("等待登录超时（%s）", timeout)
}

type cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

// cookies 通过 CDP 读取所有 cookie（包含 httpOnly）。
func (f *Flow) cookies(ctx context.Context) ([]cookie, error) {
	wsURL, err := f.pageWS(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(wsURL, fmt.Sprintf("http://127.0.0.1:%d", f.Port))
	if err != nil {
		return nil, err
	}
	conn, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	if err := websocket.Message.Send(conn, `{"id":1,"method":"Network.getAllCookies"}`); err != nil {
		return nil, err
	}
	for {
		var raw string
		if err := websocket.Message.Receive(conn, &raw); err != nil {
			return nil, err
		}
		var msg struct {
			ID     int `json:"id"`
			Result struct {
				Cookies []cookie `json:"cookies"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(raw), &msg) != nil {
			continue
		}
		if msg.ID == 1 {
			return msg.Result.Cookies, nil
		}
	}
}

// pageWS 找到当前页面的 DevTools WebSocket 地址。
func (f *Flow) pageWS(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/list", f.Port), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var targets []struct {
		Type                 string `json:"type"`
		URL                  string `json:"url"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, nil
		}
	}
	return "", fmt.Errorf("浏览器页面还没准备好")
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// ProfileDirFor 便于测试/排查：返回 profile 目录。
func (f *Flow) ProfileDirFor() string {
	if f == nil {
		return ""
	}
	return filepath.Clean(f.Profile)
}

// DumpCookies 返回 cookie 的 "name@domain" 列表（排查 CDP 是否可读）。
func (f *Flow) DumpCookies(ctx context.Context) ([]string, error) {
	cs, err := f.cookies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name+"@"+c.Domain)
	}
	return out, nil
}
