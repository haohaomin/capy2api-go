package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/haohaomin/capy2api/internal/browserlogin"
	"github.com/haohaomin/capy2api/internal/store"
)

type loginState struct {
	mu        sync.Mutex
	flow      *browserlogin.Flow
	cancel    context.CancelFunc
	status    string // idle|waiting|ok|error
	message   string
	accountID string
	name      string
}

func (s *Server) loginSnapshot() map[string]any {
	s.login.mu.Lock()
	defer s.login.mu.Unlock()
	status := s.login.status
	if status == "" {
		status = "idle"
	}
	return map[string]any{
		"status":     status,
		"message":    s.login.message,
		"account_id": s.login.accountID,
		"name":       s.login.name,
		"url":        browserlogin.SignInURL,
	}
}

// handleBrowserLogin 启动「浏览器登录」：打开 Chrome 到 Capy 登录页，等 cookie。
func (s *Server) handleBrowserLogin(w http.ResponseWriter, r *http.Request) {
	s.login.mu.Lock()
	if s.login.status == "waiting" {
		msg := s.login.message
		s.login.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "waiting", "message": msg, "url": browserlogin.SignInURL})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	flow, err := browserlogin.Start(ctx)
	if err != nil {
		s.login.mu.Unlock()
		cancel()
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.login.flow = flow
	s.login.cancel = cancel
	s.login.status = "waiting"
	s.login.message = "已打开浏览器，请在窗口里完成 Capy 登录（Google 或邮箱均可）"
	s.login.accountID = ""
	s.login.name = ""
	s.login.mu.Unlock()

	go s.waitBrowserLogin(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "waiting", "url": browserlogin.SignInURL})
}

func (s *Server) waitBrowserLogin(ctx context.Context) {
	flow := s.login.flow
	deadline := time.Now().Add(5 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		// Clerk 在未登录时也会下发匿名的 __client，所以这里要「拿到 cookie → 校验会话是否 active」，
		// 失败就继续等，而不是直接判失败。
		cookie, err := flow.WaitCookie(ctx, 8*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		vctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		acc := store.Account{Cookie: cookie, Source: "browser-login"}
		validated, _, verr := s.validateAccount(vctx, acc)
		cancel()
		if verr != nil {
			lastErr = verr
			s.login.mu.Lock()
			s.login.message = "已拿到浏览器 cookie，等待登录完成…"
			s.login.mu.Unlock()
			continue
		}
		saved, serr := s.store.Add(validated)
		if serr != nil {
			s.finishLogin("error", "保存账号失败："+serr.Error(), "", "")
			return
		}
		s.store.MarkState(saved.ID, "ok", "")
		s.invalidateModels(saved.ID)
		s.ensureDaemon(saved)
		msg := "登录成功，账号已导入"
		if saved.DeviceID == "" {
			msg = "登录成功，账号已导入；但该账号还没有设备绑定 —— thread 模式的免费执行需要设备会话（在任意一台登录过 Capy 的机器上导入一次），否则只能用 web 模式（需额度）"
		}
		s.finishLogin("ok", msg, saved.ID, firstNonEmpty(saved.Email, saved.Name))
		return
	}
	msg := "等待登录超时"
	if lastErr != nil {
		msg += "：" + lastErr.Error()
	}
	s.finishLogin("error", msg, "", "")
}

func (s *Server) finishLogin(status, message, accountID, name string) {
	s.login.mu.Lock()
	if s.login.flow != nil {
		s.login.flow.Close()
		s.login.flow = nil
	}
	if s.login.cancel != nil {
		s.login.cancel()
		s.login.cancel = nil
	}
	s.login.status = status
	s.login.message = message
	s.login.accountID = accountID
	s.login.name = name
	s.login.mu.Unlock()
}

func (s *Server) handleBrowserLoginStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.loginSnapshot())
}

func (s *Server) handleBrowserLoginCancel(w http.ResponseWriter, r *http.Request) {
	s.finishLogin("idle", "已取消", "", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
