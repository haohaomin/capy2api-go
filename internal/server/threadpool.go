// threadpool.go 根线程池：把「创建根线程」与「使用线程」解耦。
//
// 背景（实测）：
//   - 子线程会继承父线程的对话上下文 —— 不同会话之间记忆会串，不能对外；
//   - 根线程（项目 + 设备）没有父线程，互不影响；
//   - 根线程创建时设备必须在线（广告模型还要求创建来自网页/App 侧会话），
//     但**创建完成后设备离线也能继续对话**。
//
// 于是预置一个根线程池：设备在线时后台补充，请求时直接取用，用完（一次性）或
// 绑定到会话长期复用。
package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/store"
)

type threadPool struct {
	mu      sync.Mutex
	idle    []string
	lastErr string
	lastLog time.Time
}

// takeRootThreadForPrompt 直接建一条带本轮 prompt 的根线程（daemon 在线时首选）。
func (s *Server) takeRootThreadForPrompt(ctx context.Context, acc store.Account, model capy.Model, reasoning, prompt string, files []capy.FileRef) (string, error) {
	// 设备隧道可能正在重连：等它上线再建线程，别直接把 503 抛给用户。
	if d := s.daemon(); d != nil {
		waitUntil := time.Now().Add(10 * time.Second)
		for d.Status()["online"] != true && time.Now().Before(waitUntil) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if s.daemon() != nil && s.daemon().Status()["online"] == true {
		sess := s.sessionFor(acc)
		proj, err := sess.EnsureProject(ctx, acc.OrgID, "Home")
		if err != nil {
			return "", err
		}
		var jam *capy.Jam
		// 建线程是设备侧串行操作：并发打过去会互相阻塞到超时，这里用信号量排队。
		select {
		case s.createSem <- struct{}{}:
			defer func() { <-s.createSem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
		for attempt := 0; attempt < 1; attempt++ {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			jam, err = sess.CreateRootThreadLinked(cctx, acc.OrgID, proj, acc.DeviceID, s.cfg().Capy.DeviceWorkspace, "capy2api", prompt, model.ID, reasoning, files)
			cancel()
			if err == nil {
				break
			}
			if strings.Contains(err.Error(), "deadline exceeded") {
				return "", fmt.Errorf("设备忙，建线程超时（20s），请稍后重试")
			}
			if err == nil {
				break
			}
			// 设备隧道刚重连的一瞬间会报 offline：等它一下再试，别直接 503。
			if strings.Contains(err.Error(), "device is offline") || strings.Contains(err.Error(), "Device offline") {
				select {
				case <-ctx.Done():
					return "", err
				case <-time.After(2 * time.Second):
					continue
				}
			}
			return "", err
		}
		if err != nil {
			return "", err
		}
		return jam.ID, nil
	}
	// 没有 daemon 时退回池/现场建（仍需要真实设备在线）。
	jam, err := s.takeRootThread(ctx, acc, model, reasoning)
	if err != nil {
		return "", err
	}
	return jam, nil
}

// take 从池里取一条根线程；池空则尝试现场创建。
func (s *Server) takeRootThread(ctx context.Context, acc store.Account, model capy.Model, reasoning string) (string, error) {
	s.pool.mu.Lock()
	if n := len(s.pool.idle); n > 0 {
		jam := s.pool.idle[n-1]
		s.pool.idle = s.pool.idle[:n-1]
		s.pool.mu.Unlock()
		return jam, nil
	}
	s.pool.mu.Unlock()
	return s.createRootThread(ctx, acc, model, reasoning)
}

// createRootThread 新建一条根线程（设备必须在线）。
func (s *Server) createRootThread(ctx context.Context, acc store.Account, model capy.Model, reasoning string) (string, error) {
	if acc.DeviceID == "" {
		return "", errNoDevice
	}
	sess := s.sessionFor(acc)
	proj, err := sess.EnsureProject(ctx, acc.OrgID, "Home")
	if err != nil {
		return "", err
	}
	// 假设备在线时用它声明过的 linked 目录（不需要 worktree）；否则退回 worktree。
	if s.daemon() != nil && s.daemon().Status()["online"] == true {
		ws := s.cfg().Capy.DeviceWorkspace
		jam, err := sess.CreateRootThreadLinked(ctx, acc.OrgID, proj, acc.DeviceID, ws, "capy2api", "hi", model.ID, reasoning, nil)
		if err != nil {
			s.notePoolErr(err)
			return "", err
		}
		return jam.ID, nil
	}
	root := acc.WorkspaceRoot
	if root == "" {
		root = s.cfg().Capy.Workspace.Root
	}
	jam, err := sess.CreateRootThread(ctx, acc.OrgID, proj, acc.DeviceID, root, "capy2api", "hi", model.ID, reasoning, nil)
	if err != nil {
		s.notePoolErr(err)
		return "", err
	}
	return jam.ID, nil
}

func (s *Server) notePoolErr(err error) {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	s.pool.lastErr = err.Error()
	if time.Since(s.pool.lastLog) > 5*time.Minute {
		s.pool.lastLog = time.Now()
		log.Printf("WARN 根线程池补充失败（设备可能离线）：%v", err)
	}
}

// RefillLoop 后台补充根线程池。
func (s *Server) StartThreadPool(ctx context.Context) {
	if s.cfg().Capy.ThreadMode != "root" || s.cfg().Capy.PoolSize <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			s.refillPool(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) refillPool(ctx context.Context) {
	// 配了伪造设备就一律按需建线程（带真实 prompt），不再预建 "hi" 种子线程。
	if s.daemon() != nil {
		return
	}
	s.pool.mu.Lock()
	need := s.cfg().Capy.PoolSize - len(s.pool.idle)
	s.pool.mu.Unlock()
	if need <= 0 {
		return
	}
	acc, err := s.store.Next()
	if err != nil {
		return
	}
	models, err := s.catalog(ctx, acc, false)
	if err != nil || len(models) == 0 {
		return
	}
	for i := 0; i < need; i++ {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		jam, err := s.createRootThread(cctx, acc, models[0], pickReasoning(models[0], "", s.cfg().Capy.Reasoning))
		cancel()
		if err != nil {
			return
		}
		s.pool.mu.Lock()
		s.pool.idle = append(s.pool.idle, jam)
		s.pool.mu.Unlock()
		log.Printf("根线程池 +1：%s（空闲 %d/%d）", jam, len(s.pool.idle), s.cfg().Capy.PoolSize)
	}
}

func (s *Server) daemonStatus() any {
	d := s.daemon()
	if d == nil {
		return map[string]any{"enabled": false}
	}
	st := d.Status()
	st["enabled"] = true
	return st
}

func (s *Server) poolStats() map[string]any {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return map[string]any{"idle": len(s.pool.idle), "size": s.cfg().Capy.PoolSize, "last_error": s.pool.lastErr}
}
