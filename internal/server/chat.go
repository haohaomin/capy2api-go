package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/store"
)

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCallOut   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Reasoning           *reasoningOpt   `json:"reasoning,omitempty"`
	Tools               []toolDef       `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	User                string          `json:"user,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	ConversationID      string          `json:"conversation_id,omitempty"`
	ConversationIDCamel string          `json:"conversationId,omitempty"`
	PromptCacheKey      string          `json:"prompt_cache_key,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type reasoningOpt struct {
	Effort string `json:"effort"`
}

type turnOutcome struct {
	comp *completion
	err  error
}

func renderContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("content 只支持字符串或 [{type:text,text:...}] 数组")
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "", "text", "input_text", "output_text":
			b.WriteString(blk.Text)
		default:
			return "", fmt.Errorf("不支持的内容块类型 %q（桥接只处理文本）", blk.Type)
		}
	}
	return b.String(), nil
}

// messagesForPrompt 生成给工具协议看的对话 JSON。
func messagesForPrompt(messages []chatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		text, err := renderContent(m.Content)
		if err != nil {
			text = ""
		}
		item := map[string]any{"role": m.Role, "content": text}
		if len(m.ToolCalls) > 0 {
			item["tool_calls"] = m.ToolCalls
			item["content"] = ""
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			item["name"] = m.Name
		}
		out = append(out, item)
	}
	return out
}

// buildPlainPrompt 把 OpenAI messages 折成一条给 Capy agent 的文本。
func buildPlainPrompt(messages []chatMessage) (string, error) {
	if len(messages) == 0 {
		return "", errors.New("messages 不能为空")
	}
	var systems []string
	var lines []string
	userCount := 0
	lastRole := ""
	for _, m := range messages {
		text, err := renderContent(m.Content)
		if err != nil {
			return "", err
		}
		switch m.Role {
		case "system", "developer":
			systems = append(systems, text)
			continue
		case "user":
			userCount++
			lines = append(lines, "[user] "+text)
		case "assistant":
			if len(m.ToolCalls) > 0 {
				raw, _ := json.Marshal(m.ToolCalls)
				lines = append(lines, "[assistant tool_calls] "+string(raw))
			} else {
				lines = append(lines, "[assistant] "+text)
			}
		case "tool":
			lines = append(lines, "[tool result "+(m.Name+m.ToolCallID)+"] "+text)
		default:
			return "", fmt.Errorf("不支持的消息角色 %q", m.Role)
		}
		lastRole = m.Role
	}
	if userCount == 0 && lastRole != "tool" {
		return "", errors.New("messages 至少要有一条 user 消息")
	}
	if len(systems) == 0 && len(lines) == 1 && lastRole == "user" {
		single := strings.TrimPrefix(lines[0], "[user] ")
		if strings.TrimSpace(single) == "" {
			return "", errors.New("最后一条 user 消息为空")
		}
		return single, nil
	}
	var b strings.Builder
	if len(systems) > 0 {
		b.WriteString("系统指令：\n")
		b.WriteString(strings.Join(systems, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString("<对话记录>\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n</对话记录>\n\n")
	b.WriteString("请直接回答最后一条 [user] 消息。只输出回答正文：不要复述对话记录，不要输出角色标签，不要解释你的格式。")
	if lastRole == "tool" {
		b.WriteString("最后一条是工具结果，请基于它继续完成任务。")
	}
	b.WriteString("你只需要回答，不要调用终端、文件或其他平台工具。")
	return b.String(), nil
}

// --------------------------------------------------------------- chat handler

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := decodeJSON(r, &req); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeAPIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeAPIError(w, http.StatusBadRequest, "model 不能为空", "invalid_request_error")
		return
	}
	// DSH 每个会话都会单独发一条「生成标题」请求；本地直接回一个短标题，
	// 不再为它建线程/调模型（省 2 秒 + 一条线程 + 一次请求）。
	if title, ok := localTitle(req); ok {
		s.writeLocalTitle(w, r, req, title)
		return
	}

	if s.queueBlocked(w) {
		return
	}
	t := tenantOf(r)
	acc, err := s.accountFor(t)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error(), "no_account")
		return
	}
	// 会话复用：同一段对话的第二轮起复用同一条上游线程（上下文与缓存留在上游）。
	acc, boundJam, convKey, releaseConv, sysHash, busyRetry := s.conversationFor(r.Context(), &req, acc, t.ID)
	if busyRetry {
		w.Header().Set("retry-after", "1")
		writeAPIError(w, http.StatusConflict, "同一会话上一轮仍在处理中，请稍后重试（相同请求已在执行）", "conversation_busy")
		return
	}
	defer releaseConv()
	continued := boundJam != ""
	existingJam := boundJam
	models, err := s.catalog(r.Context(), acc, false)
	if err != nil {
		s.failAccount(acc, err)
		writeAPIError(w, http.StatusBadGateway, "拉取模型目录失败: "+err.Error(), "upstream_error")
		return
	}
	model, err := resolveModel(req.Model, models)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if !t.Master && len(t.Key.AllowedModels) > 0 && !containsFold(t.Key.AllowedModels, model.ID) {
		writeAPIError(w, http.StatusForbidden, "该 API key 无权使用模型 "+model.ID, "model_not_allowed")
		return
	}
	choice, err := toolChoice(req.ToolChoice, len(req.Tools) > 0)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}
	messages, images, err := collectImages(req.Messages)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	req.Messages = messages
	files, err := s.uploadImages(r.Context(), acc, images)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "图片处理失败: "+err.Error(), "invalid_image")
		return
	}
	want := req.ReasoningEffort
	if want == "" && req.Reasoning != nil {
		want = req.Reasoning.Effort
	}
	reasoning := pickReasoning(model, want, s.cfg().Capy.Reasoning)

	nonce := ""
	var prompt string
	if len(req.Tools) > 0 {
		nonce = shortID()
		prompt = buildToolPrompt(nonce, choice, parallel, req.Tools, req.Messages)
	} else {
		prompt, err = buildPlainPrompt(req.Messages)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
	}
	// 新会话：伪造设备在线时直接建一条带 prompt 的根线程（没有 seed 消息、不继承上下文）。
	promptDelivered := false
	if existingJam == "" && strings.ToLower(firstNonEmpty(acc.Mode, s.cfg().Capy.Mode, "thread")) == "thread" {
		jam, rerr := s.takeRootThreadForPrompt(r.Context(), acc, model, reasoning, prompt, files)
		if rerr == nil {
			existingJam = jam
			promptDelivered = true
			// 线程一建好就登记，正在跑的时候别的请求才能看到 jam 并打断它。
			if convKey != "" {
				s.convos.bind(convKey, jam, acc.ID, acc.OrgID, model.ID, sysHash)
			}
		} else if s.cfg().Capy.ThreadMode != "side" {
			writeAPIError(w, http.StatusServiceUnavailable,
				"根线程池为空且设备离线，无法创建隔离线程："+rerr.Error()+"（启动 Capy 桌面端补充线程池，或把 thread_mode 设为 side 接受上下文共享）",
				"no_thread_available")
			return
		}
	}
	if s.cfg().Capy.MemoryGuard {
		prompt = memoryGuardLine + "\n\n" + prompt
	}
	prompt = formatHintLine + "\n\n" + prompt
	if len(req.Tools) == 0 && strings.EqualFold(s.cfg().Capy.DevicePolicy, "text") {
		// 纯文本模式下上游没有任何文件/命令工具：明确告诉它别尝试，否则会卡在
		// 「调用工具 → 被拒绝 → 再试」的循环里，最后连正文都出不来。
		prompt = "【能力边界】本会话没有文件、shell、浏览或联网工具；不要尝试调用任何工具。需要读取文件或联网时，直接说明无法访问，然后用已有信息回答。\n\n" + prompt
	}
	if continued {
		if len(req.Tools) > 0 {
			// 工具请求：只发「上一轮之后新增的消息」，否则 tool 结果会被丢掉，
			// 模型就会说「工具结果为空」。
			prompt = buildDeltaPrompt(req.Messages, s.convos.sentCount(convKey))
		} else if text, ok := lastUserText(req.Messages); ok {
			prompt = text
		} else {
			existingJam = ""
		}
	}
	if len(images) > 0 {
		prompt += fmt.Sprintf("\n\n（用户附带了 %d 张图片，已作为附件随消息上传，请结合图片内容回答。）", len(images))
	}
	if len(prompt) > 2_000_000 {
		writeAPIError(w, http.StatusBadRequest, "对话过长（超过 200 万字符），请压缩上下文", "invalid_request_error")
		return
	}

	start := time.Now()
	requestID := "chatcmpl-" + shortID()
	created := time.Now().Unix()
	entry := store.LogEntry{
		Time:        time.Now().Format(time.RFC3339),
		Account:     acc.Name,
		Model:       model.ID,
		Status:      "error",
		PromptChars: len(prompt),
		Stream:      req.Stream,
		Tools:       len(req.Tools),
		Client:      clientLabel(r),
		Key:         t.Name,
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg().Capy.TurnTimeoutSec+40)*time.Second)
	defer cancel()

	type deltaEvent struct {
		kind string // content | reasoning
		text string
	}
	deltaCh := make(chan deltaEvent, 256)
	pushKind := func(kind, text string) {
		if text == "" {
			return
		}
		select {
		case deltaCh <- deltaEvent{kind: kind, text: text}:
		default:
		}
	}
	push := func(text string) { pushKind("content", text) }
	onReason := func(text string) { pushKind("reasoning", text) }
	onStep := func() { pushKind("step", "\n\n") }
	var onDelta, onText func(string)
	// 不管有没有 tools，最终答案都在 message_user 的 text 字段里；实时通道会逐段吐。
	onText = push
	if len(req.Tools) == 0 {
		onDelta = push
	} else {
		// 工具模式：最终答案是协议 JSON 里的 content 字段；只走一个解析器，
		// 不要同时转发原始增量，否则会把 JSON 泄漏给客户端。
		ex := &finalTextExtractor{}
		onText = func(raw string) {
			for _, piece := range ex.Feed(raw) {
				push(piece)
			}
		}
		onDelta = nil
	}
	preview := prompt
	if len([]rune(preview)) > 80 {
		preview = string([]rune(preview)[:80]) + "…"
	}
	kind := "new"
	if continued {
		if len(req.Tools) > 0 {
			kind = "delta-tools"
		} else {
			kind = "delta-text"
		}
	}
	log.Printf("提示词 rid=%s kind=%s msgs=%d len=%d jam=%s head=%q", requestID, kind, len(req.Messages), len([]rune(prompt)), existingJam, preview)
	ch := make(chan turnOutcome, 1)
	go func() {
		release, serr := s.acquireKeySlot(ctx, t)
		if serr == nil {
			defer release()
		}
		comp, cerr := s.complete(ctx, acc, model, reasoning, prompt, nonce, choice, req.Tools, parallel, files, existingJam, existingJam != "" && (continued || convKey != ""), promptDelivered, onDelta, onText, onReason, onStep)
		ch <- turnOutcome{comp: comp, err: cerr}
	}()

	finish := func(comp *completion, err error) {
		entry.DurationMS = time.Since(start).Milliseconds()
		if comp != nil && comp.Result != nil {
			log.Printf("回合耗时 rid=%s model=%s jam=%s 总=%dms 首字=%dms 生成=%dms 输出=%d tok 步骤=%d",
				requestID, model.ID, comp.Result.JamID, entry.DurationMS,
				comp.Result.TTFTMS, comp.Result.GenMS, comp.Usage.OutputTokens, comp.Result.Steps)
		} else if err != nil {
			log.Printf("回合失败 rid=%s model=%s 总=%dms err=%v", requestID, model.ID, entry.DurationMS, err)
		}
		noteRequest(model.ID, err == nil && comp != nil, req.Stream, usageOf(comp), time.Since(start), resultOf(comp))
		recordHealth(err == nil && comp != nil, time.Since(start))
		if convKey != "" {
			switch {
			case err != nil && continued:
				if s.convos.isSuperseded(convKey, existingJam) {
					// 这条线程已经被同一会话的新消息接管：不解绑、不覆盖绑定。
					log.Printf("会话 %s：本轮被新消息接管，跳过解绑", convKey)
				} else if isTimeoutErr(err) {
					if dropped := s.convos.noteFailure(convKey, false); dropped {
						log.Printf("会话 %s 连续超时，已解绑并将在下一轮重建线程", convKey)
					}
				} else {
					s.convos.dropIf(convKey, existingJam)
				}
			case err == nil && comp != nil:
				s.convos.bind(convKey, existingJam, acc.ID, acc.OrgID, model.ID, sysHash)
				s.convos.setSentCount(convKey, len(req.Messages))
			}
		}
		if err != nil || comp == nil {
			if err != nil {
				entry.Error = err.Error()
			}
			// 失败也计一次调用，避免靠报错绕过请求数配额。
			if !t.Master {
				s.keys.Record(t.ID, 0, 0)
			}
			s.log.Push(entry)
			return
		}
		s.markQueue(false)
		entry.Status = "ok"
		entry.JamID = comp.Result.JamID
		if !t.Master {
			s.keys.Record(t.ID, comp.Usage.InputTokens, comp.Usage.OutputTokens)
		}
		entry.PromptTokens = comp.Usage.InputTokens
		entry.OutputTokens = comp.Usage.OutputTokens
		entry.CacheTokens = comp.Usage.CacheReadTokens
		if comp.Result.Ad != nil {
			entry.AdBrand = comp.Result.Ad.BrandName
		}
		s.log.Push(entry)
	}

	if !req.Stream {
		select {
		case out := <-ch:
			if out.err != nil {
				finish(nil, out.err)
				if convKey != "" && existingJam != "" && s.convos.currentJam(convKey) != existingJam {
					writeAPIError(w, http.StatusConflict, "已被同一会话的新消息接管", "conversation_superseded")
					return
				}
				s.writeUpstreamError(w, out.err)
				return
			}
			finish(out.comp, nil)
			msg := map[string]any{"role": "assistant", "content": out.comp.Content}
			if s.cfg().Capy.IncludeReasoning && out.comp.Result != nil && out.comp.Result.Reasoning != "" {
				msg["reasoning_content"] = out.comp.Result.Reasoning
			}
			if len(out.comp.Calls) > 0 {
				msg["content"] = nil
				msg["tool_calls"] = out.comp.Calls
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":      requestID,
				"object":  "chat.completion",
				"created": created,
				"model":   model.ID,
				"choices": []map[string]any{{
					"index":         0,
					"message":       msg,
					"finish_reason": out.comp.Reason,
				}},
				"usage": usageBlock(out.comp.Usage),
				"capy":  out.comp.Trace,
			})
		case <-r.Context().Done():
			finish(nil, r.Context().Err())
		}
		return
	}

	// 流式：先发 SSE 头，等上游回合时用注释心跳保活。
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(payload string) bool {
		if _, err := io.WriteString(w, payload); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	write(": capy2api\n\n")

	emit := func(delta map[string]any, finishReason any, usage map[string]any, extra ...map[string]any) bool {
		choices := []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}}
		if delta == nil && usage != nil {
			choices = []map[string]any{}
		}
		payload := map[string]any{
			"id":      requestID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model.ID,
			"choices": choices,
		}
		if usage != nil {
			payload["usage"] = usage
		}
		for _, e := range extra {
			for k, v := range e {
				payload[k] = v
			}
		}
		raw, _ := json.Marshal(payload)
		return write("data: " + string(raw) + "\n\n")
	}

	sentText := ""
	emitDelta := func(text string) bool {
		if text == "" {
			return true
		}
		// 上游实时通道常把一段攒成一个大块；再切小片下发，客户端显示更连续。
		runes := []rune(text)
		for start := 0; start < len(runes); start += 12 {
			end := start + 12
			if end > len(runes) {
				end = len(runes)
			}
			piece := string(runes[start:end])
			if !emit(map[string]any{"content": piece}, nil, nil) {
				return false
			}
			sentText += piece
			if end < len(runes) {
				time.Sleep(4 * time.Millisecond)
			}
		}
		return true
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			finish(nil, r.Context().Err())
			return
		case ev := <-deltaCh:
			if ev.kind == "step" {
				// 新一步：在思考流里插个分隔，客户端就能看出「一段思考 → 工具/正文」的边界。
				emit(map[string]any{"reasoning_content": "\n\n"}, nil, nil)
				continue
			}
			if ev.kind == "reasoning" {
				if !emit(map[string]any{"reasoning_content": ev.text}, nil, nil) {
					finish(nil, errors.New("客户端断开"))
					return
				}
				continue
			}
			// 真流式：把实时通道的增量直接下发（工具 JSON 的 text 字段也已解析成纯文本）。
			if !emitDelta(ev.text) {
				finish(nil, errors.New("客户端断开"))
				return
			}
		case <-ticker.C:
			if !write(": keepalive\n\n") {
				finish(nil, errors.New("客户端断开"))
				return
			}
		case out := <-ch:
			if out.err != nil {
				finish(nil, out.err)
				if convKey != "" && existingJam != "" && s.convos.currentJam(convKey) != existingJam {
					writeSSEError(write, "已被同一会话的新消息接管", "conversation_superseded")
					return
				}
				writeSSEError(write, out.err.Error(), "upstream_error")
				return
			}
			finish(out.comp, nil)
			emit(map[string]any{"role": "assistant", "content": ""}, nil, nil)
			if len(out.comp.Calls) > 0 {
				// 先发 id/name，再把 arguments 切片成增量片段（对齐原生 function calling 的流式形态）。
				for i, c := range out.comp.Calls {
					emit(map[string]any{"tool_calls": []map[string]any{{
						"index": i, "id": c.ID, "type": c.Type,
						"function": map[string]any{"name": c.Function.Name, "arguments": ""},
					}}}, nil, nil)
					args := []rune(c.Function.Arguments)
					const chunk = 24
					for start := 0; start < len(args); start += chunk {
						end := start + chunk
						if end > len(args) {
							end = len(args)
						}
						emit(map[string]any{"tool_calls": []map[string]any{{
							"index":    i,
							"function": map[string]any{"arguments": string(args[start:end])},
						}}}, nil, nil)
					}
				}
			} else {
				// 与已下发的增量对账：最终文本通常以已发送内容为前缀，只补剩余部分。
				content := out.comp.Content
				switch {
				case strings.HasPrefix(content, sentText):
					if rest := content[len(sentText):]; rest != "" {
						if !emitDelta(rest) {
							return
						}
					}
				case sentText == "":
					for _, piece := range chunkText(content, s.cfg().Capy.Typewriter) {
						if !emitDelta(piece) {
							return
						}
					}
				default:
					// 增量与最终文本不一致（模型中途改口）：只补公共前缀之后的部分，避免重复。
					common := 0
					minLen := len(content)
					if len(sentText) < minLen {
						minLen = len(sentText)
					}
					for common < minLen && content[common] == sentText[common] {
						common++
					}
					if rest := content[common:]; rest != "" {
						if !emitDelta(rest) {
							return
						}
					}
				}
			}
			emit(map[string]any{}, out.comp.Reason, nil, map[string]any{"capy": out.comp.Trace})
			if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
				emit(nil, nil, usageBlock(out.comp.Usage))
			}
			write("data: [DONE]\n\n")
			return
		}
	}
}

// writeSSEError 输出一条 OpenAI 风格的流式错误并收尾。
func writeSSEError(write func(string) bool, message, code string) {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}})
	write("data: " + string(raw) + "\n\n")
	write("data: [DONE]\n\n")
}

// chunkText 把最终文本切成增量片段（客户端体验用；上游本身不吐增量）。
func chunkText(text string, enabled bool) []string {
	if !enabled {
		return []string{text}
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	size := len(runes) / 160
	if size < 6 {
		size = 6
	}
	out := make([]string, 0, len(runes)/size+1)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func usageBlock(u capy.Usage) map[string]any {
	prompt := u.InputTokens
	completion := u.OutputTokens
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.CacheReadTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": u.ReasoningTokens,
		},
	}
}

func (s *Server) traceBlock(res *capy.TurnResult, reasoning string) map[string]any {
	block := map[string]any{
		"jam_id":            res.JamID,
		"duration_ms":       res.Duration.Milliseconds(),
		"polls":             res.Polls,
		"steps":             res.Steps,
		"stop_reason":       res.StopReason,
		"reasoning":         reasoning,
		"usage":             res.Usage,
		"cache_hit_ratio":   res.CacheHitRatio,
		"ttft_ms":           res.TTFTMS,
		"gen_ms":            res.GenMS,
		"tokens_per_second": res.TokPerSec,
		"delta_chars":       res.DeltaChars,
		"transport":         res.Transport,
	}
	if res.ContextTokens != nil {
		block["context_tokens"] = res.ContextTokens
	}
	if res.ProviderModel != "" {
		block["provider_model"] = res.ProviderModel
	}
	if res.Route != nil {
		block["route"] = res.Route
	}
	if res.Ad != nil {
		block["ad"] = res.Ad
	}
	if res.RTUsage != nil {
		block["realtime_usage"] = res.RTUsage
	}
	if len(res.ToolsUsed) > 0 {
		block["tools_used"] = res.ToolsUsed
	}
	if res.ContextTokens != nil {
		block["mcp_tokens"] = res.ContextTokens.MCP
		block["skills_tokens"] = res.ContextTokens.Skills
		block["rules_tokens"] = res.ContextTokens.Rules
	}
	if res.Reasoning != "" {
		block["reasoning"] = res.Reasoning
	}
	if len(res.StepsDetail) > 0 {
		block["steps_detail"] = res.StepsDetail
	}
	if res.Jam != nil {
		block["jam"] = res.Jam
	}
	return block
}

// runTurn 走一次完整上游回合（受全局并发限制）。
func (s *Server) runTurn(ctx context.Context, acc store.Account, model capy.Model, reasoning, prompt, title string, files []capy.FileRef, existingJam string, keepThread, promptDelivered bool, onDelta, onText, onReason func(string), onStep func()) (*capy.TurnResult, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	mode := strings.ToLower(firstNonEmpty(acc.Mode, s.cfg().Capy.Mode, "thread"))
	if mode == "device" && strings.TrimSpace(acc.DeviceID) == "" {
		return nil, errors.New("device 模式需要设备 id：确认 Capy 桌面端已登录后重新导入，或把 mode 改成 web")
	}
	wsMode := acc.WorkspaceMode
	if wsMode == "" {
		wsMode = s.cfg().Capy.Workspace.Mode
	}
	root := acc.WorkspaceRoot
	if root == "" {
		root = s.cfg().Capy.Workspace.Root
	}
	sess := s.sessionFor(acc)
	res, err := sess.RunTurn(ctx, capy.TurnOptions{
		Mode:            mode,
		TemplateJam:     s.cfg().Capy.TemplateJam,
		ProjectID:       firstNonEmpty(acc.ProjectID, s.cfg().Capy.ProjectID),
		Org:             acc.OrgID,
		DeviceID:        acc.DeviceID,
		WorkspaceMode:   wsMode,
		WorkspaceRoot:   root,
		Model:           model.ID,
		Reasoning:       reasoning,
		Prompt:          prompt,
		Title:           title,
		Timeout:         time.Duration(s.cfg().Capy.TurnTimeoutSec) * time.Second,
		Poll:            time.Duration(s.cfg().Capy.PollIntervalMS) * time.Millisecond,
		DeleteAfter:     s.cfg().Capy.DeleteAfter,
		Files:           files,
		ExistingJam:     existingJam,
		KeepThread:      keepThread,
		PromptDelivered: promptDelivered,
		OnDelta:         onDelta,
		OnText:          onText,
		OnReason:        onReason,
		OnStep:          onStep,
		Realtime:        s.cfg().Capy.Realtime,
	})
	if err != nil {
		s.failAccount(acc, err)
	}
	return res, err
}

// failAccount 记录失败并给账号安排冷却（避免同一个坏账号被连续打穿）。
func (s *Server) failAccount(acc store.Account, err error) {
	status := capy.StatusOf(err)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		s.store.MarkState(acc.ID, "invalid", err.Error())
		s.invalidateModels(acc.ID)
	case status == http.StatusTooManyRequests:
		s.store.MarkCooldown(acc.ID, 60*time.Second, err.Error())
	case status >= 500:
		s.store.MarkCooldown(acc.ID, 30*time.Second, err.Error())
	case status == 0:
		// 传输层失败（DNS/TLS/连接被重置）：短冷却，别把池子拖进循环重试。
		s.store.MarkCooldown(acc.ID, 20*time.Second, err.Error())
	default:
		s.store.MarkState(acc.ID, acc.State, err.Error())
	}
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "上游排队中") {
		s.markQueue(true)
		w.Header().Set("retry-after", "30")
		writeAPIError(w, http.StatusTooManyRequests, err.Error(), "upstream_queued")
		return
	}
	if strings.Contains(err.Error(), "工具决策") {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error(), "invalid_tool_decision")
		return
	}
	var pause *capy.UpstreamPause
	if errors.As(err, &pause) {
		switch pause.Code {
		case "insufficient_credits":
			writeAPIError(w, http.StatusPaymentRequired, pause.Message+"（web 模式需要 Capy 额度；没有额度时把 capy.mode 改成 device，用本机 Capy 桌面端执行）", "insufficient_credits")
		default:
			writeAPIError(w, http.StatusConflict, pause.Message, firstNonEmpty(pause.Code, "upstream_paused"))
		}
		return
	}
	status := capy.StatusOf(err)
	switch {
	case status == http.StatusTooManyRequests:
		writeAPIError(w, http.StatusTooManyRequests, err.Error(), "rate_limited")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		writeAPIError(w, http.StatusBadGateway, "Capy 账号会话失效："+err.Error(), "upstream_auth_error")
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, "上游回合超时", "upstream_timeout")
	case errors.Is(err, context.Canceled):
		writeAPIError(w, 499, "客户端已断开", "client_closed")
	default:
		writeAPIError(w, http.StatusBadGateway, err.Error(), "upstream_error")
	}
}

// localTitle 识别客户端的「生成标题」请求并本地生成标题，返回 (标题, 是否命中)。
func localTitle(req chatRequest) (string, bool) {
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		text, err := renderContent(m.Content)
		if err != nil || !strings.Contains(strings.ToLower(text), "concise title") {
			return "", false
		}
		last := ""
		for _, x := range req.Messages {
			if x.Role == "user" {
				if t, e := renderContent(x.Content); e == nil {
					last = t
				}
			}
		}
		title := strings.TrimSpace(strings.Split(strings.Split(last, "\n")[0], "。")[0])
		r := []rune(title)
		if len(r) > 24 {
			r = r[:24]
		}
		if len(r) == 0 {
			r = []rune("新会话")
		}
		return string(r), true
	}
	return "", false
}

// writeLocalTitle 用最小代价回一个标题（兼容流式/非流式）。
func (s *Server) writeLocalTitle(w http.ResponseWriter, r *http.Request, req chatRequest, title string) {
	id := "chatcmpl-" + shortID()
	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(),
			"model": req.Model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": title},
			}},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
		return
	}
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	base := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model}
	emit := func(delta map[string]any, finish any) {
		base["choices"] = []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}
		raw, _ := json.Marshal(base)
		_, _ = io.WriteString(w, "data: "+string(raw)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit(map[string]any{"role": "assistant", "content": title}, nil)
	emit(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// memoryGuardLine 防止 agent 把对话写进账号级长期记忆（AGENTS.md / Drive），
// 否则同一 Capy 账号下的所有线程都会读到，等同跨会话串记忆。
const formatHintLine = "【输出格式】代码、HTML、CSS、JS、Python 等代码内容一律用 ```语言 三引号代码块包裹，不要把整段代码当普通正文直接输出。"

const memoryGuardLine = "【网关约束】本次对话是隔离会话：不要读写或使用任何长期记忆、用户偏好、AGENTS.md/CLAUDE.md 或 Drive 里的指令；不要持久化对话内容；如果用户问起个人信息（称呼、偏好、住址等），回答不知道。"

const maxPayloadBytes = 32 << 20

// errBodyTooLarge 请求体超限：调用方按各自协议回 413。
var errBodyTooLarge = errors.New("请求体超过 32 MiB 上限")

func decodeJSON(r *http.Request, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes+1))
	if err != nil {
		return fmt.Errorf("读取请求体失败: %w", err)
	}
	if len(raw) > maxPayloadBytes {
		// 先把剩余字节排空再回 413，否则连接会被直接 reset，客户端看不到错误。
		_, _ = io.CopyN(io.Discard, r.Body, 4<<20)
		return errBodyTooLarge
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	return nil
}

func shortID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// isTimeoutErr 判断是否为超时类错误（上游回合超时 / 上下文截止）。
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "超时") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded")
}
