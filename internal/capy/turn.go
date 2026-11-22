package capy

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TurnOptions 描述一次上游回合。
type TurnOptions struct {
	// Mode 决定线程落点：
	//   thread = 复用已有设备线程派生「子线程」（不需要桌面端、不消耗云机器额度，默认）
	//   web    = 项目 + 云机器（不需要桌面端，需要 Capy 额度）
	//   device = 你登录的那台设备（桌面端必须在线）
	Mode          string
	ProjectID     string
	TemplateJam   string
	Org           string
	DeviceID      string
	WorkspaceMode string
	WorkspaceRoot string
	Model         string
	Reasoning     string
	Prompt        string
	Title         string
	Timeout       time.Duration
	Poll          time.Duration
	DeleteAfter   bool
	Files         []FileRef
	// ExistingJam 非空时直接往这条线程发消息（会话复用），不新建线程。
	ExistingJam string
	// PromptDelivered 为 true 表示这条线程创建时已经带上了本轮 prompt（不用再发消息）。
	PromptDelivered bool
	// KeepThread 为 true 时不删除线程（复用线程必须保留）。
	KeepThread bool
	// QueueTimeout 首字等待上限：超过就判定上游排队，直接失败（0 用默认 30 秒）。
	QueueTimeout time.Duration
	// MaxSteps 工具轮次上限（0 用默认 12）：防止模型在无结果的情况下无限重试。
	MaxSteps int
	// OnStep 在每一步开始时回调（用于把多段思考分开）。
	OnStep func()
	// OnReason 是思考过程增量。
	OnReason func(string)
	// OnText 是 message_user 工具里的可见文本增量（工具模式的最终答案）。
	OnText func(string)
	// OnDelta 文本增量回调（真流式下发用；为 nil 时只做计时）。
	OnDelta func(string)
	// Realtime 是否订阅实时增量通道（首字延时/生成速度靠它；失败自动退回轮询）。
	Realtime bool
}

// StepInfo 是单步（一次 assistant 落地）的明细。
type StepInfo struct {
	Index         int      `json:"index"`
	EventID       string   `json:"event_id,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	StopReason    string   `json:"stop_reason,omitempty"`
	Usage         Usage    `json:"usage"`
	ModelID       string   `json:"model_id,omitempty"`
	ProviderModel string   `json:"provider_model,omitempty"`
	ReasoningMode string   `json:"reasoning_mode,omitempty"`
	Blocks        []string `json:"blocks,omitempty"`
}

// JamMeta 是线程元数据。
type JamMeta struct {
	Title           string `json:"title,omitempty"`
	Preview         string `json:"preview,omitempty"`
	ParentJamID     string `json:"parent_jam_id,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	ThreadNumber    int    `json:"thread_number,omitempty"`
	CreditsContinue bool   `json:"credits_continue"`
	Origin          string `json:"origin,omitempty"`
}

// TurnResult 是一次成功的回合结果。
type TurnResult struct {
	ContextTokens *ContextTokens `json:"context_tokens,omitempty"`
	ProviderModel string         `json:"provider_model,omitempty"`
	Route         *RouteRef      `json:"route,omitempty"`
	ToolsUsed     []string       `json:"tools_used,omitempty"`
	CacheHitRatio float64        `json:"cache_hit_ratio"`
	JamID         string         `json:"jam_id"`
	Text          string         `json:"text"`
	Usage         Usage          `json:"usage"`
	Steps         int            `json:"steps"`
	StopReason    string         `json:"stop_reason"`
	Ad            *Ad            `json:"ad,omitempty"`
	Duration      time.Duration  `json:"duration"`
	Polls         int            `json:"polls"`
	// 实时通道观测：首字延时、生成窗口、生成速度、增量字符数、通道类型。
	TTFTMS     int64   `json:"ttft_ms"`
	GenMS      int64   `json:"gen_ms"`
	TokPerSec  float64 `json:"tokens_per_second"`
	DeltaChars int     `json:"delta_chars"`
	Transport  string  `json:"transport"`
	// RTUsage 是实时通道 step-finish 给的 usage（字段更全，含 cache write）。
	RTUsage *Usage `json:"realtime_usage,omitempty"`
	// Reasoning 是上游 thinking 块的内容（模型没给就是空）。
	Reasoning string `json:"reasoning,omitempty"`
	// StepsDetail 是每个 assistant 步骤的用量/时间/模型明细。
	StepsDetail []StepInfo `json:"steps_detail,omitempty"`
	// Jam 是线程元数据（标题、预览、项目、父线程等）。
	Jam *JamMeta `json:"jam,omitempty"`
}

// RunTurn 创建线程、等待回答、清理线程。
//
// 每个请求独立线程：不复用其他请求的对话历史，回答只看本次 prompt。
func (s *Session) RunTurn(ctx context.Context, opts TurnOptions) (*TurnResult, error) {
	if strings.TrimSpace(opts.Prompt) == "" {
		return nil, errors.New("prompt 为空")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 240 * time.Second
	}
	if opts.Poll < 200*time.Millisecond {
		opts.Poll = 700 * time.Millisecond
	}
	jamID := "jam_" + NewULID()
	res := &TurnResult{JamID: jamID}
	started := time.Now()

	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = "device"
	}
	device := &Device{
		DeviceID: opts.DeviceID,
		Workspace: Workspace{
			Kind: opts.WorkspaceMode,
			Root: opts.WorkspaceRoot,
		},
	}
	if mode == "device" {
		if device.Workspace.Kind == "" {
			device.Workspace.Kind = "worktree"
		}
		if device.Workspace.Kind == "worktree" {
			device.Workspace.Root = filepath.Join(opts.WorkspaceRoot, jamID)
			// 父目录先建好；具体线程目录由设备侧创建，提前建会让设备拒绝。
			_ = os.MkdirAll(opts.WorkspaceRoot, 0o755)
		}
	}

	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = "capy2api"
	}
	req := CreateJamRequest{
		JamID:     jamID,
		RequestID: newUUID(),
		Title:     title,
		Message:   opts.Prompt,
		Model:     ModelSelection{ModelID: opts.Model, ReasoningMode: opts.Reasoning},
		Device:    device,
		Files:     opts.Files,
	}

	turnCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var (
		rt           *Realtime
		promptSentAt time.Time
		turnStarted  time.Time
	)
	startRealtime := func() {
		if !opts.Realtime {
			return
		}
		token, terr := s.Token(turnCtx)
		if terr != nil {
			return
		}
		rctx, rcancel := context.WithCancel(context.WithoutCancel(turnCtx))
		client, derr := DialRealtime(rctx, token, jamID)
		if derr != nil {
			rcancel()
			return
		}
		client.onGrow = opts.OnDelta
		client.onText = opts.OnText
		client.onReason = opts.OnReason
		client.onStep = opts.OnStep
		rt = client
		go func() {
			client.Loop(rctx)
			rcancel()
		}()
	}
	defer func() {
		if rt != nil {
			rt.Close()
		}
	}()

	var baseline map[string]bool
	if strings.TrimSpace(opts.ExistingJam) != "" {
		jamID = strings.TrimSpace(opts.ExistingJam)
		res.JamID = jamID
		startRealtime()
		turnStarted = time.Now()
		promptSentAt = turnStarted
		// 记录发送前的已提交事件：新回合只能认「基线之后」的 commit，
		// 否则上游还没收尾时会把上一轮的答案当成这一轮。
		baseline = map[string]bool{}
		if prev, perr := s.AllEvents(turnCtx, opts.Org, jamID); perr == nil {
			for i := range prev {
				if prev[i].Kind == "assistant" && prev[i].State == "committed" {
					baseline[prev[i].ID] = true
				}
			}
		}
		if !opts.PromptDelivered {
			if serr := s.SendPrompt(turnCtx, opts.Org, jamID, opts.Prompt, opts.Model, opts.Reasoning, opts.Files); serr != nil {
				return nil, fmt.Errorf("发送消息失败: %w", serr)
			}
		}
	} else {
		switch mode {
		case "thread":
			template, terr := s.EnsureTemplateJam(turnCtx, opts.Org, opts.TemplateJam)
			if terr != nil {
				return nil, terr
			}
			child, cerr := s.CreateSideThread(turnCtx, opts.Org, template)
			if cerr != nil {
				cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
				return nil, fmt.Errorf("派生子线程失败: %w", cerr)
			}
			jamID = child
			res.JamID = child
			startRealtime()
			promptSentAt = time.Now()
			if serr := s.SendPrompt(turnCtx, opts.Org, child, opts.Prompt, opts.Model, opts.Reasoning, opts.Files); serr != nil {
				cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
				return nil, fmt.Errorf("发送消息失败: %w", serr)
			}
		case "web":
			projectID := strings.TrimSpace(opts.ProjectID)
			if projectID == "" {
				pid, perr := s.EnsureProject(turnCtx, opts.Org, "Home")
				if perr != nil {
					return nil, perr
				}
				projectID = pid
			}
			if _, err := s.CreateJamInProject(turnCtx, opts.Org, projectID, req); err != nil {
				InvalidateProject(opts.Org)
				cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
				return nil, fmt.Errorf("创建云端线程失败: %w", err)
			}
			promptSentAt = time.Now()
			startRealtime()
		default:
			req.Device = device
			if _, err := s.CreateJam(turnCtx, opts.Org, req); err != nil {
				cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
				return nil, fmt.Errorf("创建设备线程失败: %w", err)
			}
			promptSentAt = time.Now()
			startRealtime()
		}
	}

	interval := opts.Poll
	jamChecks := 0
	var lastEvents []Event
	lastProgress := time.Now()
	lastEventCount, lastChars := -1, -1
	for {
		select {
		case <-turnCtx.Done():
			cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
			if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("上游回合超时（%s）", opts.Timeout)
			}
			return nil, turnCtx.Err()
		case <-time.After(interval):
		}
		res.Polls++
		events, err := s.AllEvents(turnCtx, opts.Org, jamID)
		if err != nil {
			if StatusOf(err) == 404 {
				cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
				return nil, fmt.Errorf("线程已被上游回收: %w", err)
			}
			interval = bump(interval, opts.Poll)
			continue
		}
		if len(baseline) > 0 {
			filtered := events[:0:0]
			for i := range events {
				if events[i].Kind == "assistant" && baseline[events[i].ID] {
					continue
				}
				filtered = append(filtered, events[i])
			}
			events = filtered
		}
		chars := -1
		if rt != nil {
			chars = rt.Snapshot().Chars
		}
		if len(events) != lastEventCount || chars != lastChars {
			lastEventCount, lastChars = len(events), chars
			lastProgress = time.Now()
		} else if time.Since(lastProgress) > 120*time.Second {
			cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
			return nil, fmt.Errorf("回合 120 秒没有任何进展（可能卡在工具重试），已中止")
		}
		if pause := EventError(events); pause != nil {
			cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
			return nil, pause
		}
		qto := opts.QueueTimeout
		if qto <= 0 {
			qto = 30 * time.Second
		}
		charsNow := -1
		if rt != nil {
			charsNow = rt.Snapshot().Chars
		}
		if charsNow <= 0 && countAssistantSince(events, turnStarted) == 0 && time.Since(turnStarted) > qto {
			cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
			return nil, fmt.Errorf("上游排队中：%.0f 秒没有开始输出，稍后重试", qto.Seconds())
		}
		if n := countNativeToolUse(events, turnStarted); n > 8 {
			cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
			return nil, fmt.Errorf("纯文本模式下无法读取文件/执行命令（已拦截 %d 次工具尝试）", n)
		}
		if maxSteps := opts.MaxSteps; true {
			if maxSteps <= 0 {
				maxSteps = 12
			}
			if n := countToolSteps(events, turnStarted); n > maxSteps {
				cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
				return nil, fmt.Errorf("工具调用轮次超限（%d > %d），已中止以防死循环", n, maxSteps)
			}
		}
		if text, usage, stop, steps, ok := ExtractAnswerSince(events, turnStarted); ok {
			lastEvents = events
			res.Text = text
			res.Usage = usage
			res.StopReason = stop
			res.Steps = steps
			break
		}
		jamChecks++
		if jamChecks%3 == 0 {
			jam, err := s.GetJam(turnCtx, opts.Org, jamID)
			if err == nil {
				switch strings.ToLower(jam.RunStatus) {
				case "error", "cancelled", "failed":
					cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
					return nil, fmt.Errorf("上游回合状态异常: %s", jam.RunStatus)
				}
			}
		}
		interval = opts.Poll
	}

	// 等线程真正回到 idle 再返回：否则下一条消息（delivery=interrupt）可能打在
	// 还没收尾的回合上被吞掉，表现为「回复了上一轮的内容」。
	// waitIdle 已停用：baseline 事件过滤已能避免把上一轮答案当成这一轮。

	res.ContextTokens, res.ProviderModel, res.Route = LastStepMeta(lastEvents)
	res.ToolsUsed = ToolsUsed(lastEvents)
	res.Reasoning, res.StepsDetail = ExtractDetail(lastEvents)
	applyTiming(res, rt, promptSentAt, lastEvents)
	if res.Usage.InputTokens > 0 {
		res.CacheHitRatio = float64(res.Usage.CacheReadTokens) / float64(res.Usage.InputTokens)
	}
	if jam, jerr := s.GetJam(turnCtx, opts.Org, jamID); jerr == nil {
		res.Jam = &JamMeta{Title: jam.Title, Preview: jam.Preview, ParentJamID: jam.ParentJamID,
			Origin: jam.Origin, ThreadNumber: jam.ThreadNo, CreditsContinue: jam.CreditsCont}
		if jam.ProjectID != nil {
			res.Jam.ProjectID = *jam.ProjectID
		}
	}
	s.mu.Lock()
	if s.adCache == nil {
		s.adCache = map[string]*Ad{}
	}
	cachedAd, adOK := s.adCache[jamID]
	s.mu.Unlock()
	var ad *Ad
	if adOK {
		ad = cachedAd
	} else if fetched, err := s.ThreadAd(turnCtx, opts.Org, jamID); err == nil {
		ad = fetched
		s.mu.Lock()
		s.adCache[jamID] = ad
		s.mu.Unlock()
	}
	if ad != nil {
		res.Ad = ad
	}
	res.Duration = time.Since(started)
	cleanupTurn(context.WithoutCancel(ctx), s, opts, jamID)
	return res, nil
}

func cleanupTurn(ctx context.Context, s *Session, opts TurnOptions, jamID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	// 复用线程时不打断：回合已经结束，interrupt 只会让线程多忙一会儿，
	// 下一条请求就可能被判 busy 而另起线程。
	if !opts.KeepThread {
		_ = s.Interrupt(ctx, opts.Org, jamID)
	}
	if opts.DeleteAfter && !opts.KeepThread {
		_, _ = s.DeleteJams(ctx, opts.Org, []string{jamID})
		if strings.ToLower(opts.Mode) == "device" && opts.WorkspaceMode != "linked" {
			dir := filepath.Join(opts.WorkspaceRoot, jamID)
			if strings.HasPrefix(filepath.Base(dir), "jam_") {
				_ = os.RemoveAll(dir)
			}
		}
	}
}

// waitIdle 最多等 5 秒，直到 jam 的 runStatus 回到 idle/done。
func waitIdle(ctx context.Context, s *Session, org, jamID string) {
	for i := 0; i < 25; i++ {
		jam, err := s.GetJam(ctx, org, jamID)
		if err == nil {
			st := strings.ToLower(jam.RunStatus)
			if st == "idle" || st == "done" || st == "" {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	log.Printf("WARN 线程 %s 落地后 5 秒仍未 idle", jamID)
}

// countAssistantSince 统计本轮以来 assistant 事件数（判断是否真的开始产出）。
func countAssistantSince(events []Event, since time.Time) int {
	n := 0
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, ev.CreatedAt); err == nil && ts.Before(since) {
			continue
		}
		n++
	}
	return n
}

// countNativeToolUse 统计本轮 agent 自己发起的原生工具调用（read/bash/write…）。
// 纯文本策略下这些调用都会被拒，模型却可能反复重试，需要设个上限尽早失败。
func countNativeToolUse(events []Event, since time.Time) int {
	n := 0
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" || ev.Payload == nil {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, ev.CreatedAt); err == nil && ts.Before(since) {
			continue
		}
		for _, b := range ev.Payload.Blocks {
			if b.Type != "tool_use" || b.ToolName == "message_user" {
				continue
			}
			switch b.ToolName {
			case "read", "bash", "write", "edit", "glob", "grep":
				n++
			}
		}
	}
	return n
}

// countToolSteps 统计本轮 assistant 步骤数（含工具调用轮次）。
func countToolSteps(events []Event, since time.Time) int {
	n := 0
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, ev.CreatedAt); err == nil && ts.Before(since) {
			continue
		}
		n++
	}
	return n
}

// anyCommittedSince 判断 since 之后是否有 committed 的 assistant 事件。
func anyCommittedSince(events []Event, since time.Time) bool {
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" || ev.State != "committed" {
			continue
		}
		if ev.CreatedAt == "" {
			return true
		}
		if ts, err := time.Parse(time.RFC3339Nano, ev.CreatedAt); err == nil {
			if !ts.Before(since) {
				return true
			}
			continue
		}
		return true
	}
	return false
}

// ToolsUsed 统计本轮调用过的平台工具（去重，排除 message_user）。
func ToolsUsed(events []Event) []string {
	seen := map[string]bool{}
	var out []string
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" || ev.Payload == nil {
			continue
		}
		for _, b := range ev.Payload.Blocks {
			if b.Type != "tool_use" || b.ToolName == "" || b.ToolName == "message_user" {
				continue
			}
			if !seen[b.ToolName] {
				seen[b.ToolName] = true
				out = append(out, b.ToolName)
			}
		}
	}
	return out
}

// LastStepMeta 取最后一步的上下文构成、真实上游模型与路由。
func LastStepMeta(events []Event) (*ContextTokens, string, *RouteRef) {
	var ctx *ContextTokens
	var provider string
	var route *RouteRef
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" || ev.Payload == nil {
			continue
		}
		if ev.Payload.ContextTokens != nil {
			ctx = ev.Payload.ContextTokens
		}
		if ev.Payload.Model != nil && ev.Payload.Model.ProviderModelID != "" {
			provider = ev.Payload.Model.ProviderModelID
		}
		if ev.Payload.Route != nil {
			route = ev.Payload.Route
		}
	}
	return ctx, provider, route
}

// ExtractAnswerSince 只认「本轮开始之后」才落地的 assistant 事件。
//
// 复用线程时事件是累加的：不过滤就会把上一轮的 committed 回答当成本轮结果。
// 留 1.5s 余量吸收本机与上游的时钟偏差。
func ExtractAnswerSince(events []Event, since time.Time) (string, Usage, string, int, bool) {
	if since.IsZero() {
		return ExtractAnswer(events)
	}
	cutoff := since.Add(-1500 * time.Millisecond)
	filtered := make([]Event, 0, len(events))
	for _, ev := range events {
		if ev.Kind != "assistant" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, ev.CreatedAt); err == nil && t.Before(cutoff) {
			continue
		}
		filtered = append(filtered, ev)
	}
	return ExtractAnswer(filtered)
}

// ExtractAnswer 从事件列表里取出最终回答。
//
// 规则：只看 state=committed 的 assistant 事件；平台把最终答复放在
// message_user 工具调用（state=final）里时优先用它，否则拼接 text 块。
// thinking 块永不进入回答。
func ExtractAnswer(events []Event) (string, Usage, string, int, bool) {
	var usage Usage
	var lastStep Usage
	outSum, reasoningSum := 0, 0
	steps := 0
	answer := ""
	texts := make([]string, 0, 4)
	stop := ""
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" || ev.State != "committed" || ev.Payload == nil {
			continue
		}
		steps++
		if ev.Payload.Usage != nil {
			// 输入/缓存取「最后一步」的快照：多步回合里每步都会重发上下文，
			// 累加会把 44k 算成 132k。输出 token 才需要跨步累加。
			lastStep = *ev.Payload.Usage
			outSum += ev.Payload.Usage.OutputTokens
			reasoningSum += ev.Payload.Usage.ReasoningTokens
		}
		var parts []string
		for _, b := range ev.Payload.Blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					parts = append(parts, b.Text)
				}
			case "tool_use":
				if b.ToolName == "message_user" && b.Args != nil {
					if b.Args.State == "" || b.Args.State == "final" {
						if strings.TrimSpace(b.Args.Text) != "" {
							answer = b.Args.Text
						}
					}
				}
			}
		}
		if len(parts) > 0 {
			texts = append(texts, strings.Join(parts, ""))
		}
		if ev.Payload.StopReason != "" {
			stop = ev.Payload.StopReason
		}
	}
	if answer == "" && len(texts) > 0 {
		answer = texts[len(texts)-1]
	}
	if lastStep.InputTokens > 0 || lastStep.OutputTokens > 0 {
		// 输入取最后一步；输出跨步累加。
		usage = lastStep
		usage.OutputTokens = outSum
		usage.ReasoningTokens = reasoningSum
	}
	if strings.TrimSpace(answer) == "" {
		return "", usage, stop, steps, false
	}
	if stop == "tool_use" && answer == "" {
		stop = "stop"
	}
	return answer, usage, stop, steps, true
}

func bump(cur, base time.Duration) time.Duration {
	next := cur * 2
	if next > 4*time.Second {
		next = 4 * time.Second
	}
	if next < base {
		next = base
	}
	return next
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID 生成 26 位 ULID（Capy 线程 id 的格式）。
func NewULID() string {
	var out [26]byte
	now := uint64(time.Now().UnixMilli())
	for i := 9; i >= 0; i-- {
		out[i] = crockford[now%32]
		now /= 32
	}
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	for i := 0; i < 16; i++ {
		out[10+i] = crockford[buf[i]%32]
	}
	return string(out[:])
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// applyTiming 计算首字延时、生成窗口与生成速度。
//
// 优先用实时通道（真·首字）；没有实时通道时退回事件时间戳：
// assistant.startedAt 是模型开始，event.createdAt 是落地时间，两者之差就是生成窗口。
func applyTiming(res *TurnResult, rt *Realtime, promptSentAt time.Time, events []Event) {
	res.Transport = "poll"
	start, commit := LastStepTiming(events)
	var firstAt, lastAt time.Time
	if rt != nil {
		// step-finish（带 usage）可能比 REST 的 committed 晚到：等一小会儿再收口。
		deadline := time.Now().Add(700 * time.Millisecond)
		var snap Snapshot
		for {
			snap = rt.Snapshot()
			if snap.Usage.InputTokens > 0 || snap.Usage.OutputTokens > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !snap.FirstAt.IsZero() {
			firstAt, lastAt = snap.FirstAt, snap.LastAt
			res.DeltaChars = snap.Chars
			res.Transport = "realtime"
			if snap.Usage.InputTokens > 0 || snap.Usage.OutputTokens > 0 {
				u := snap.Usage
				res.RTUsage = &u
			}
		}
	}
	if firstAt.IsZero() {
		if !start.IsZero() && !commit.IsZero() && commit.After(start) {
			firstAt, lastAt = start, commit
		}
	}
	// 用 REST 的落地时间当生成窗口右端：增量可能只覆盖 JSON 尾巴，直接用最后一条
	// 增量会把生成速度算得虚高。
	if !firstAt.IsZero() && !commit.IsZero() && commit.After(firstAt) {
		lastAt = commit
	}
	if !firstAt.IsZero() && !promptSentAt.IsZero() && firstAt.After(promptSentAt) {
		res.TTFTMS = firstAt.Sub(promptSentAt).Milliseconds()
	}
	if !firstAt.IsZero() && !lastAt.IsZero() && lastAt.After(firstAt) {
		res.GenMS = lastAt.Sub(firstAt).Milliseconds()
	}
	// 增量窗口太短（首字几乎与落地同时到）时，除出来的速度没有意义，直接不报。
	if res.GenMS >= 300 && res.Usage.OutputTokens >= 8 {
		tps := float64(res.Usage.OutputTokens) / (float64(res.GenMS) / 1000)
		// 夹一个合理上限：窗口太小时算出来的「20 万 tok/s」没有意义。
		if tps > 0 && tps < 5000 {
			res.TokPerSec = tps
		}
	}
}

// LastStepTiming 返回最后一步的模型开始时间与事件落地时间。
func LastStepTiming(events []Event) (time.Time, time.Time) {
	var start, commit time.Time
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" || ev.Payload == nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, ev.Payload.StartedAt); err == nil {
			start = t
		}
		if t, err := time.Parse(time.RFC3339Nano, ev.CreatedAt); err == nil {
			commit = t
		}
	}
	return start, commit
}

// ExtractDetail 提取 thinking 文本与每个 assistant 步骤的明细。
func ExtractDetail(events []Event) (string, []StepInfo) {
	var reasoning []string
	steps := make([]StepInfo, 0, 2)
	for i := range events {
		ev := events[i]
		if ev.Kind != "assistant" || ev.Payload == nil {
			continue
		}
		info := StepInfo{Index: len(steps), EventID: ev.ID, StartedAt: ev.Payload.StartedAt, CreatedAt: ev.CreatedAt, StopReason: ev.Payload.StopReason}
		if ev.Payload.Usage != nil {
			info.Usage = *ev.Payload.Usage
		}
		if ev.Payload.Model != nil {
			info.ModelID = ev.Payload.Model.ModelID
			info.ProviderModel = ev.Payload.Model.ProviderModelID
			info.ReasoningMode = ev.Payload.Model.ReasoningMode
		}
		for _, b := range ev.Payload.Blocks {
			info.Blocks = append(info.Blocks, b.Type)
			if b.Type == "thinking" && strings.TrimSpace(b.Text) != "" {
				reasoning = append(reasoning, b.Text)
			}
		}
		steps = append(steps, info)
	}
	return strings.Join(reasoning, "\n\n"), steps
}
