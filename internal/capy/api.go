package capy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Model 是 /app/models 返回的模型条目（只保留桥接需要的字段）。
type Model struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	Provider        string             `json:"provider"`
	Auth            string             `json:"auth"`
	BYOK            bool               `json:"byok"`
	Ads             *bool              `json:"ads"`
	Pricing         map[string]float64 `json:"pricing"`
	Route           *Route             `json:"route"`
	Limits          *Limits            `json:"limits"`
	Efforts         []string           `json:"efforts"`
	DefaultEffort   string             `json:"defaultEffort"`
	Unavailable     any                `json:"unavailable"`
	KnowledgeCutoff any                `json:"knowledgeCutoff"`
}

// Route 描述模型的路由资格。
type Route struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Label   string `json:"label"`
	Locked  bool   `json:"locked"`
	Skipped any    `json:"skipped"`
}

// Limits 是上下文/输出上限。
type Limits struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// FirstParty 判断是否为当前账号可用的、免费（广告赞助）的 Capy 模型。
func (m Model) FirstParty() bool {
	if m.Ads == nil || !*m.Ads {
		return false
	}
	if m.Auth != "capy" || m.BYOK {
		return false
	}
	if m.Unavailable != nil {
		return false
	}
	if m.Route == nil || m.Route.Kind != "capy" || m.Route.Locked || m.Route.Skipped != nil {
		return false
	}
	for _, key := range []string{"input", "output", "cacheRead"} {
		v, ok := m.Pricing[key]
		if !ok || v != 0 {
			return false
		}
	}
	return true
}

// RouteRef 是实际承接推理的路由。
type RouteRef struct {
	Source     string `json:"source"`
	ProviderID string `json:"providerId"`
}

// Usage 是一轮对话的 token 统计。
type Usage struct {
	InputTokens          int `json:"inputTokens"`
	OutputTokens         int `json:"outputTokens"`
	CacheReadTokens      int `json:"cacheReadTokens"`
	CacheWriteTokens     int `json:"cacheWriteTokens"`
	ReasoningTokens      int `json:"reasoningTokens"`
	NonCachedInputTokens int `json:"nonCachedInputTokens,omitempty"`
	TotalTokens          int `json:"totalTokens,omitempty"`
}

// Add 累加多步 usage。
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// Block 是 assistant 消息里的一块内容。
type Block struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	Text     string    `json:"text"`
	ToolName string    `json:"toolName"`
	Args     *ToolArgs `json:"args"`
}

// ToolArgs 是 message_user 之类平台工具的入参。
type ToolArgs struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// ContextTokens 是上游给的上下文构成（system/tools/mcp/…），用于解释输入 token。
type ContextTokens struct {
	System       int `json:"system"`
	Tools        int `json:"tools"`
	Rules        int `json:"rules"`
	Skills       int `json:"skills"`
	MCP          int `json:"mcp"`
	Environment  int `json:"environment"`
	Conversation int `json:"conversation"`
}

// EventPayload 是事件 payload 中桥接关心的部分。
type EventPayload struct {
	Blocks     []Block `json:"blocks"`
	Usage      *Usage  `json:"usage"`
	StopReason string  `json:"stopReason"`
	Model      *struct {
		ModelID         string `json:"modelId"`
		ProviderModelID string `json:"providerModelId"`
		ReasoningMode   string `json:"reasoningMode"`
	} `json:"model"`
	Route         *RouteRef      `json:"route"`
	ContextTokens *ContextTokens `json:"contextTokens"`
	StartedAt     string         `json:"startedAt"`
	Text          string         `json:"text"`
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	Retryable     bool           `json:"retryable"`
}

// Event 是 /jams/{id}/events 的条目。
type Event struct {
	ID        string        `json:"id"`
	JamID     string        `json:"jamId"`
	Kind      string        `json:"kind"`
	State     string        `json:"state"`
	Payload   *EventPayload `json:"payload"`
	CreatedAt string        `json:"createdAt"`
}

// EventsPage 是事件分页响应。
type EventsPage struct {
	Items  []Event `json:"items"`
	Cursor *string `json:"cursor"`
}

// Jam 是线程概要。
type Jam struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	RunStatus   string  `json:"runStatus"`
	Status      string  `json:"status"`
	ProjectID   *string `json:"projectId"`
	LastModelID string  `json:"lastModelId"`
	ParentJamID string  `json:"parentJamId"`
	Archived    bool    `json:"archived"`
	Preview     string  `json:"preview"`
	Origin      string  `json:"origin"`
	ThreadNo    int     `json:"threadNumber"`
	CreditsCont bool    `json:"creditsContinue"`
	Model       *struct {
		ModelID       string `json:"modelId"`
		ReasoningMode string `json:"reasoningMode"`
	} `json:"model"`
	PrimaryMachine *MachineRef `json:"primaryMachine"`
}

// MachineRef 是线程的执行机器。
type MachineRef struct {
	Kind      string `json:"kind"`
	MachineID string `json:"machineId"`
	Name      string `json:"name"`
}

// Device 描述执行线程的设备绑定。
type Device struct {
	DeviceID  string    `json:"deviceId"`
	Workspace Workspace `json:"workspace"`
}

// Workspace 是设备侧目录策略。
type Workspace struct {
	Kind            string   `json:"kind"`
	Root            string   `json:"root"`
	Base            string   `json:"base,omitempty"`
	Repos           []string `json:"repos,omitempty"`
	StartFromOrigin bool     `json:"startFromOrigin"`
}

// ModelSelection 是请求里的模型选择。
type ModelSelection struct {
	ModelID       string `json:"modelId"`
	ReasoningMode string `json:"reasoningMode,omitempty"`
}

// CreateJamRequest 是 POST /app/orgs/{org}/jams 的请求体。
type CreateJamRequest struct {
	JamID     string         `json:"jamId"`
	RequestID string         `json:"requestId"`
	Title     string         `json:"title,omitempty"`
	Message   string         `json:"message"`
	Model     ModelSelection `json:"model"`
	Device    *Device        `json:"device,omitempty"`
	Files     []FileRef      `json:"files,omitempty"`
	ClientKey string         `json:"clientKey,omitempty"`
	Personal  *bool          `json:"personal,omitempty"`
}

// DeleteOutcome 是删除线程的结果条目。
type DeleteOutcome struct {
	JamID   string `json:"jamId"`
	Outcome string `json:"outcome"`
}

// Ad 是线程广告（广告赞助模型的计费凭据）。
type Ad struct {
	AdText    string `json:"adText"`
	Title     string `json:"title"`
	BrandName string `json:"brandName"`
	URL       string `json:"url"`
	CTA       string `json:"cta"`
}

// ------------------------------------------------------------------ endpoints

// Me 返回当前用户。
func (s *Session) Me(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := s.Do(ctx, "GET", "/app/me", "", nil, &out)
	return out, err
}

// Organizations 返回账号所属组织。
func (s *Session) Organizations(ctx context.Context) ([]Organization, error) {
	var raw any
	if err := s.Do(ctx, "GET", "/app/organizations", "", nil, &raw); err != nil {
		return nil, err
	}
	items, err := asList(raw)
	if err != nil {
		return nil, err
	}
	out := make([]Organization, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		org := Organization{}
		org.ID, _ = m["id"].(string)
		org.Name, _ = m["name"].(string)
		if v, ok := m["defaultProjectId"].(string); ok {
			org.DefaultProjectID = v
		}
		out = append(out, org)
	}
	return out, nil
}

// Organization 是组织概要。
type Organization struct {
	ID               string
	Name             string
	DefaultProjectID string
}

// Models 拉取模型目录并只保留免费广告模型。
func (s *Session) Models(ctx context.Context, org string) ([]Model, error) {
	path := "/app/models"
	if org != "" {
		path += "?orgId=" + url.QueryEscape(org)
	}
	var raw any
	if err := s.Do(ctx, "GET", path, org, nil, &raw); err != nil {
		return nil, err
	}
	items, err := asList(raw)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(items))
	for _, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var m Model
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// CreateJam 创建一条设备线程；message 即首条用户消息。
func (s *Session) CreateJam(ctx context.Context, org string, req CreateJamRequest) (*Jam, error) {
	var jam Jam
	err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/jams", org), org, req, &jam)
	if err != nil {
		return nil, err
	}
	return &jam, nil
}

// GetJam 读取线程状态。
func (s *Session) GetJam(ctx context.Context, org, jamID string) (*Jam, error) {
	var jam Jam
	err := s.Do(ctx, "GET", fmt.Sprintf("/app/orgs/%s/jams/%s", org, jamID), org, nil, &jam)
	if err != nil {
		return nil, err
	}
	return &jam, nil
}

// Events 读取线程事件；cursor 非空时继续翻页。
func (s *Session) Events(ctx context.Context, org, jamID, cursor string) (*EventsPage, error) {
	path := fmt.Sprintf("/app/orgs/%s/jams/%s/events", org, jamID)
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	var page EventsPage
	err := s.Do(ctx, "GET", path, org, nil, &page)
	if err != nil {
		return nil, err
	}
	return &page, nil
}

// AllEvents 拉取完整事件列表（跟随 cursor 翻页，最多 50 页）。
func (s *Session) AllEvents(ctx context.Context, org, jamID string) ([]Event, error) {
	var all []Event
	cursor := ""
	for i := 0; i < 50; i++ {
		page, err := s.Events(ctx, org, jamID, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Cursor == nil || *page.Cursor == "" || *page.Cursor == cursor {
			return all, nil
		}
		cursor = *page.Cursor
	}
	return all, nil
}

// DeleteJams 删除线程。
func (s *Session) DeleteJams(ctx context.Context, org string, ids []string) ([]DeleteOutcome, error) {
	var out struct {
		Outcomes []DeleteOutcome `json:"outcomes"`
	}
	err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/jams/delete", org), org,
		map[string]any{"jamIds": ids}, &out)
	if err != nil {
		return nil, err
	}
	return out.Outcomes, nil
}

// Interrupt 打断正在运行的回合。
func (s *Session) Interrupt(ctx context.Context, org, jamID string) error {
	return s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/jams/%s/interrupt", org, jamID), org, map[string]any{}, nil)
}

// ThreadAd 读取本轮线程的广告内容（广告赞助模型的展示凭据）。
func (s *Session) ThreadAd(ctx context.Context, org, jamID string) (*Ad, error) {
	var ad Ad
	if err := s.Do(ctx, "GET", fmt.Sprintf("/app/orgs/%s/jams/%s/ad", org, jamID), org, nil, &ad); err != nil {
		return nil, err
	}
	return &ad, nil
}

// SendMessage 向已有线程追加消息（用于复用线程的低频场景）。
func (s *Session) SendMessage(ctx context.Context, org, jamID string, body map[string]any) (map[string]any, error) {
	var out map[string]any
	err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/jams/%s/message", org, jamID), org, body, &out)
	return out, err
}

// asList 兼容数组与 {items:[...]} 两种返回形态。
func asList(raw any) ([]any, error) {
	switch v := raw.(type) {
	case []any:
		return v, nil
	case map[string]any:
		if items, ok := v["items"].([]any); ok {
			return items, nil
		}
		return nil, errors.New("响应里没有 items")
	default:
		return nil, errors.New("响应形态无法识别")
	}
}

// WaitJamIdle 阻塞直到线程进入 idle（或超时/失败）。
func (s *Session) WaitJamIdle(ctx context.Context, org, jamID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		jam, err := s.GetJam(ctx, org, jamID)
		if err != nil {
			return err
		}
		switch strings.ToLower(jam.RunStatus) {
		case "idle", "done", "complete", "finished":
			return nil
		case "error", "failed":
			return fmt.Errorf("上游回合失败: %s", jam.RunStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("等待线程空闲超时")
}
