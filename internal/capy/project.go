package capy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Project 是组织下的项目（网页端线程的落地位置）。
type Project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Code    string `json:"code"`
	Deleted bool   `json:"deleted"`
	Repos   []any  `json:"repos"`
}

// UpstreamPause 是上游主动暂停/失败的事件（例如云机器缺额度）。
type UpstreamPause struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *UpstreamPause) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("上游暂停[%s]: %s", e.Code, e.Message)
	}
	return "上游暂停: " + e.Code
}

// projectCache 按组织缓存 EnsureProject 的结果。
var (
	projectMu    sync.Mutex
	projectCache = map[string]projectEntry{}
)

type projectEntry struct {
	id string
	at time.Time
}

// ListProjects 读取组织的项目列表。
func (s *Session) ListProjects(ctx context.Context, org string) ([]Project, error) {
	var raw any
	if err := s.Do(ctx, "GET", fmt.Sprintf("/app/orgs/%s/projects", org), org, nil, &raw); err != nil {
		return nil, err
	}
	items, err := asList(raw)
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		p := Project{}
		p.ID, _ = m["id"].(string)
		p.Name, _ = m["name"].(string)
		p.Code, _ = m["code"].(string)
		p.Deleted, _ = m["deleted"].(bool)
		if repos, ok := m["repos"].([]any); ok {
			p.Repos = repos
		}
		out = append(out, p)
	}
	return out, nil
}

// projectCode 由项目名生成 3–4 位大写字母代码（服务端只接受字母）。
func projectCode(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
		if b.Len() >= 4 {
			break
		}
	}
	code := b.String()
	for len(code) < 3 {
		code += "Y"
	}
	return code
}

// EnsureProject 返回一个可用的项目 id；没有就新建一个（网页端线程必须有项目）。
func (s *Session) EnsureProject(ctx context.Context, org, want string) (string, error) {
	key := org + "|" + want
	projectMu.Lock()
	if e, ok := projectCache[key]; ok && time.Since(e.at) < 10*time.Minute && e.id != "" {
		projectMu.Unlock()
		return e.id, nil
	}
	projectMu.Unlock()

	projects, err := s.ListProjects(ctx, org)
	if err == nil {
		for _, p := range projects {
			if p.Deleted {
				continue
			}
			if strings.EqualFold(p.Name, want) {
				projectMu.Lock()
				projectCache[key] = projectEntry{id: p.ID, at: time.Now()}
				projectMu.Unlock()
				return p.ID, nil
			}
		}
		for _, p := range projects {
			if !p.Deleted && len(p.Repos) == 0 {
				projectMu.Lock()
				projectCache[key] = projectEntry{id: p.ID, at: time.Now()}
				projectMu.Unlock()
				return p.ID, nil
			}
		}
	}

	body := map[string]any{
		"name":       want,
		"code":       projectCode(want),
		"requestKey": "capy2api:" + org,
		"repos":      []any{},
	}
	var created Project
	if err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/projects", org), org, body, &created); err != nil {
		return "", fmt.Errorf("创建项目失败: %w", err)
	}
	if created.ID == "" {
		return "", errors.New("创建项目失败：上游未返回项目 id")
	}
	projectMu.Lock()
	projectCache[key] = projectEntry{id: created.ID, at: time.Now()}
	projectMu.Unlock()
	return created.ID, nil
}

// InvalidateProject 清掉项目缓存（项目被删时）。
func InvalidateProject(org string) {
	projectMu.Lock()
	for k := range projectCache {
		if strings.HasPrefix(k, org+"|") {
			delete(projectCache, k)
		}
	}
	projectMu.Unlock()
}

// CreateJamInProject 在指定项目里建线程（云端机器，不依赖桌面端）。
func (s *Session) CreateJamInProject(ctx context.Context, org, projectID string, req CreateJamRequest) (*Jam, error) {
	req.Device = nil
	var jam Jam
	err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/projects/%s/jams", org, projectID), org, req, &jam)
	if err != nil {
		return nil, err
	}
	return &jam, nil
}

// EventError 从事件流里取上游暂停/失败（例如 insufficient_credits）。
func EventError(events []Event) *UpstreamPause {
	for i := range events {
		ev := events[i]
		if ev.Kind != "error" {
			continue
		}
		pause := &UpstreamPause{}
		if ev.Payload != nil {
			pause.Code = ev.Payload.Code
			pause.Message = ev.Payload.Message
			pause.Retryable = ev.Payload.Retryable
		}
		if pause.Code == "" {
			pause.Code = "upstream_error"
		}
		return pause
	}
	return nil
}
