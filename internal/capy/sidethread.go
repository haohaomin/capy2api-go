package capy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 侧线程（side thread）模式：
//
// 设备线程（device jam）本身要求设备在线，但它的「子线程」不走这个校验。
// 于是可以拿任意一条已有设备线程当模板，每个请求派生一条子线程，
// 由云端跑模型推理（广告模型免费），完全不需要桌面端、也不消耗云机器额度。

var (
	templateMu    sync.Mutex
	templateCache = map[string]templateEntry{}
)

type templateEntry struct {
	id string
	at time.Time
}

// ListJams 读取组织的线程列表（新→旧）。
func (s *Session) ListJams(ctx context.Context, org string, limit int) ([]Jam, error) {
	if limit <= 0 {
		limit = 50
	}
	path := fmt.Sprintf("/app/orgs/%s/jams?limit=%d", org, limit)
	var raw any
	if err := s.Do(ctx, "GET", path, org, nil, &raw); err != nil {
		return nil, err
	}
	items, err := asList(raw)
	if err != nil {
		return nil, err
	}
	out := make([]Jam, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		jam := Jam{}
		jam.ID, _ = m["id"].(string)
		jam.Title, _ = m["title"].(string)
		jam.ParentJamID, _ = m["parentJamId"].(string)
		if v, ok := m["archived"].(bool); ok {
			jam.Archived = v
		}
		if pm, ok := m["primaryMachine"].(map[string]any); ok {
			jam.PrimaryMachine = &MachineRef{}
			jam.PrimaryMachine.Kind, _ = pm["kind"].(string)
			jam.PrimaryMachine.MachineID, _ = pm["machineId"].(string)
			jam.PrimaryMachine.Name, _ = pm["name"].(string)
		}
		out = append(out, jam)
	}
	return out, nil
}

// EnsureTemplateJam 找一条可复用的设备线程当模板。
func (s *Session) EnsureTemplateJam(ctx context.Context, org, preferred string) (string, error) {
	if strings.TrimSpace(preferred) != "" {
		return strings.TrimSpace(preferred), nil
	}
	templateMu.Lock()
	if e, ok := templateCache[org]; ok && time.Since(e.at) < 10*time.Minute && e.id != "" {
		templateMu.Unlock()
		return e.id, nil
	}
	templateMu.Unlock()

	jams, err := s.ListJams(ctx, org, 50)
	if err != nil {
		return "", err
	}
	var fallback string
	for _, j := range jams {
		if j.Archived || strings.TrimSpace(j.ID) == "" {
			continue
		}
		if j.ParentJamID != "" {
			continue // 子线程不当模板
		}
		if j.PrimaryMachine != nil && j.PrimaryMachine.Kind == "device" {
			templateMu.Lock()
			templateCache[org] = templateEntry{id: j.ID, at: time.Now()}
			templateMu.Unlock()
			return j.ID, nil
		}
		if fallback == "" {
			fallback = j.ID
		}
	}
	if fallback == "" {
		return "", errors.New("账号里没有可复用的线程：先在任意设备上开一条 Capy 线程（一次即可），或指定 template_jam")
	}
	templateMu.Lock()
	templateCache[org] = templateEntry{id: fallback, at: time.Now()}
	templateMu.Unlock()
	return fallback, nil
}

// CreateSideThread 在模板线程下派生一条子线程。
func (s *Session) CreateSideThread(ctx context.Context, org, parentJam string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	body := map[string]any{"requestId": newUUID()}
	if err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/jams/%s/side-threads", org, parentJam), org, body, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", errors.New("上游没有返回子线程 id")
	}
	return out.ID, nil
}

// SendPrompt 向线程发一条用户消息（delivery=interrupt 立即开跑）。
func (s *Session) SendPrompt(ctx context.Context, org, jamID, text, modelID, reasoning string, files []FileRef) error {
	// files 为空时上游接受空数组；有图时模型直接看见图，不需要设备去读文件。
	fileList := make([]any, 0, len(files))
	for _, f := range files {
		fileList = append(fileList, f)
	}
	body := map[string]any{
		"requestId":      newUUID(),
		"text":           text,
		"files":          fileList,
		"delivery":       "interrupt",
		"clientKey":      newUUID(),
		"clientId":       "capy2api-bridge",
		"mentions":       []any{},
		"clientFeatures": []string{"message-edit", "visual", "ads-models"},
	}
	if modelID != "" {
		sel := map[string]any{"modelId": modelID}
		if reasoning != "" {
			sel["reasoningMode"] = reasoning
		}
		body["model"] = sel
	}
	_, err := s.SendMessage(ctx, org, jamID, body)
	return err
}
