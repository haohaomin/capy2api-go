package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/haohaomin/capy2api/internal/store"
)

// ---------------------------------------------------------------- 请求结构

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice,omitempty"`
	Thinking *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
}

// anthropicToChat 把 Anthropic messages 折成统一的 chatMessage，并把图片块抽出来。
func anthropicToChat(req *anthropicRequest) ([]chatMessage, []imageInput, error) {
	var out []chatMessage
	var images []imageInput
	if len(req.System) > 0 && string(req.System) != "null" {
		text, err := anthropicText(req.System)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(text) != "" {
			raw, _ := json.Marshal(text)
			out = append(out, chatMessage{Role: "system", Content: raw})
		}
	}
	for _, m := range req.Messages {
		if len(m.Content) == 0 || string(m.Content) == "null" {
			continue
		}
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			raw, _ := json.Marshal(plain)
			out = append(out, chatMessage{Role: m.Role, Content: raw})
			continue
		}
		var blocks []anthropicBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, nil, fmt.Errorf("Anthropic content 只能是字符串或块数组: %w", err)
		}
		var texts []string
		var calls []toolCallOut
		for _, b := range blocks {
			switch b.Type {
			case "text", "":
				texts = append(texts, b.Text)
			case "thinking", "redacted_thinking":
				// Claude Code 会把思考块原样回传；我们不需要它进 prompt，忽略即可。
			case "tool_use":
				args := string(b.Input)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				call := toolCallOut{ID: firstNonEmpty(b.ID, "call_"+shortID()), Type: "function"}
				call.Function.Name = b.Name
				call.Function.Arguments = args
				calls = append(calls, call)
			case "image":
				img, err := anthropicImage(b)
				if err != nil {
					return nil, nil, err
				}
				images = append(images, img)
				texts = append(texts, fmt.Sprintf("[图片%d]", len(images)))
			case "tool_result":
				text, err := anthropicText(b.Content)
				if err != nil {
					return nil, nil, err
				}
				raw, _ := json.Marshal(text)
				out = append(out, chatMessage{Role: "tool", Content: raw, ToolCallID: b.ToolUseID})
			default:
				return nil, nil, fmt.Errorf("不支持的 Anthropic 块类型 %q", b.Type)
			}
		}
		if len(texts) > 0 || len(calls) > 0 {
			raw, _ := json.Marshal(strings.Join(texts, "\n"))
			out = append(out, chatMessage{Role: m.Role, Content: raw, ToolCalls: calls})
		}
	}
	return out, images, nil
}

// anthropicImage 解析 Anthropic 的 base64 / url 图片源。
func anthropicImage(b anthropicBlock) (imageInput, error) {
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(b.Source, &src); err != nil {
		return imageInput{}, fmt.Errorf("图片 source 解析失败: %w", err)
	}
	switch src.Type {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(src.Data)
		if err != nil {
			return imageInput{}, fmt.Errorf("图片 base64 解码失败: %w", err)
		}
		media := src.MediaType
		if media == "" {
			media = "image/png"
		}
		return imageInput{Data: data, MediaType: media}, nil
	case "url":
		if src.URL == "" {
			return imageInput{}, fmt.Errorf("图片 url 为空")
		}
		return loadImage(context.Background(), src.URL)
	default:
		return imageInput{}, fmt.Errorf("不支持的图片 source 类型 %q", src.Type)
	}
}

func anthropicText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Type == "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("无法解析文本内容")
}

func toToolDefs(list []anthropicTool) []toolDef {
	out := make([]toolDef, 0, len(list))
	for _, t := range list {
		def := toolDef{Type: "function"}
		def.Function.Name = t.Name
		def.Function.Description = t.Description
		if len(t.InputSchema) > 0 {
			def.Function.Parameters = t.InputSchema
		} else {
			def.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, def)
	}
	return out
}

// ---------------------------------------------------------------- handler

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := decodeJSON(r, &req); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeAnthropicError(w, status, err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "model 不能为空")
		return
	}
	acc, err := s.store.Next()
	if err != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	models, err := s.catalog(r.Context(), acc, false)
	if err != nil {
		s.failAccount(acc, err)
		writeAnthropicError(w, http.StatusBadGateway, "拉取模型目录失败: "+err.Error())
		return
	}
	model, err := resolveModel(req.Model, models)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	messages, images, err := anthropicToChat(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	files, err := s.uploadImages(r.Context(), acc, images)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "图片处理失败: "+err.Error())
		return
	}
	tools := toToolDefs(req.Tools)
	var rawChoice json.RawMessage
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto":
			rawChoice = json.RawMessage(`"auto"`)
		case "any":
			rawChoice = json.RawMessage(`"required"`)
		case "none":
			rawChoice = json.RawMessage(`"none"`)
		case "tool":
			rawChoice, _ = json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": req.ToolChoice.Name}})
		}
	}
	choice, err := toolChoice(rawChoice, len(tools) > 0)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	reasoning := pickReasoning(model, "", s.cfg().Capy.Reasoning)

	nonce := ""
	var prompt string
	if len(tools) > 0 {
		nonce = shortID()
		prompt = buildToolPrompt(nonce, choice, true, tools, messages)
	} else {
		prompt, err = buildPlainPrompt(messages)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	start := time.Now()
	entry := store.LogEntry{
		Time:        time.Now().Format(time.RFC3339),
		Account:     acc.Name,
		Model:       model.ID,
		Status:      "error",
		PromptChars: len(prompt),
		Stream:      req.Stream,
		Tools:       len(tools),
		Client:      clientLabel(r),
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg().Capy.TurnTimeoutSec+40)*time.Second)
	defer cancel()

	if len(images) > 0 {
		prompt += fmt.Sprintf("\n\n（用户附带了 %d 张图片，已作为附件随消息上传，请结合图片内容回答。）", len(images))
	}
	stopReason := func(comp *completion) string {
		if len(comp.Calls) > 0 {
			return "tool_use"
		}
		return "end_turn"
	}
	msgID := "msg_" + shortID()

	finishLog := func(comp *completion, cerr error) {
		if comp != nil {
			names := make([]string, 0, len(comp.Calls))
			for _, c := range comp.Calls {
				names = append(names, c.Function.Name)
			}
			log.Printf("anthropic 回合: content=%d字 calls=%v err=%v", len([]rune(comp.Content)), names, cerr)
		}
		entry.DurationMS = time.Since(start).Milliseconds()
		noteRequest(model.ID, cerr == nil, req.Stream, usageOf(comp), time.Since(start), resultOf(comp))
		recordHealth(cerr == nil, time.Since(start))
		if cerr != nil {
			entry.Error = cerr.Error()
			s.log.Push(entry)
			return
		}
		entry.Status = "ok"
		entry.JamID = comp.Result.JamID
		entry.PromptTokens = comp.Usage.InputTokens
		entry.OutputTokens = comp.Usage.OutputTokens
		entry.CacheTokens = comp.Usage.CacheReadTokens
		if comp.Result.Ad != nil {
			entry.AdBrand = comp.Result.Ad.BrandName
		}
		s.log.Push(entry)
	}

	// 非流式：同步拿结果。
	if !req.Stream {
		comp, cerr := s.complete(ctx, acc, model, reasoning, prompt, nonce, choice, tools, true, files, "", false, false, nil, nil, nil, nil)
		finishLog(comp, cerr)
		if cerr != nil {
			writeAnthropicError(w, http.StatusBadGateway, cerr.Error())
			return
		}
		content := make([]map[string]any, 0, 1+len(comp.Calls))
		if strings.TrimSpace(comp.Content) != "" {
			content = append(content, map[string]any{"type": "text", "text": comp.Content})
		}
		for _, c := range comp.Calls {
			var input any = map[string]any{}
			_ = json.Unmarshal([]byte(c.Function.Arguments), &input)
			content = append(content, map[string]any{
				"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": input,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model.ID,
			"content":       content,
			"stop_reason":   stopReason(comp),
			"stop_sequence": nil,
			"usage": anthropicUsage(comp.Usage.InputTokens, comp.Usage.OutputTokens,
				comp.Usage.CacheReadTokens, comp.Usage.CacheWriteTokens),
			"capy": comp.Trace,
		})
		return
	}

	// 流式：后台跑回合，增量边到边发（工具模式用 finalTextExtractor 只解 content 字段）。
	type deltaEvent struct {
		kind string
		text string
	}
	deltaCh := make(chan deltaEvent, 256)
	pushKind := func(kind, text string) {
		if text == "" && kind != "step" {
			return
		}
		select {
		case deltaCh <- deltaEvent{kind: kind, text: text}:
		default:
		}
	}
	push := func(text string) { pushKind("content", text) }
	// 只有客户端显式开启 thinking 时才下发思考块；否则 Claude Code 会因为
	// 出现未请求的 thinking 块而丢弃正文。
	var onReason func(string)
	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		onReason = func(text string) { pushKind("reasoning", text) }
	}
	onStep := func() { pushKind("step", "") }
	var onDelta, onText func(string)
	if len(tools) == 0 {
		onDelta = push
	} else {
		onText = push
		ex := &finalTextExtractor{}
		onDelta = func(raw string) {
			for _, piece := range ex.Feed(raw) {
				push(piece)
			}
		}
	}
	type outcome struct {
		comp *completion
		err  error
	}
	outCh := make(chan outcome, 1)
	go func() {
		c, e := s.complete(ctx, acc, model, reasoning, prompt, nonce, choice, tools, true, files, "", false, false, onDelta, onText, onReason, onStep)
		outCh <- outcome{c, e}
	}()

	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(event string, data map[string]any) bool {
		raw, _ := json.Marshal(data)
		if _, err := io.WriteString(w, "event: "+event+"\ndata: "+string(raw)+"\n\n"); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": model.ID,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropicUsage(0, 0, 0, 0),
		},
	})
	var sent strings.Builder
	nextIndex := 0
	curKind := ""
	curIndex := -1
	closeBlock := func() {
		if curKind != "" {
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": curIndex})
			curKind = ""
			curIndex = -1
		}
	}
	ensureBlock := func(kind string) bool {
		if curKind == kind {
			return true
		}
		closeBlock()
		curIndex = nextIndex
		nextIndex++
		var block map[string]any
		switch kind {
		case "thinking":
			block = map[string]any{"type": "thinking", "thinking": ""}
		default:
			block = map[string]any{"type": "text", "text": ""}
		}
		if !send("content_block_start", map[string]any{
			"type": "content_block_start", "index": curIndex, "content_block": block,
		}) {
			return false
		}
		curKind = kind
		return true
	}
	emitThinking := func(piece string) bool {
		if piece == "" {
			return true
		}
		if !ensureBlock("thinking") {
			return false
		}
		return send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": curIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": piece},
		})
	}
	emitText := func(piece string) bool {
		if piece == "" {
			return true
		}
		if !ensureBlock("text") {
			return false
		}
		runes := []rune(piece)
		for start := 0; start < len(runes); start += 12 {
			end := start + 12
			if end > len(runes) {
				end = len(runes)
			}
			part := string(runes[start:end])
			if !send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": curIndex,
				"delta": map[string]any{"type": "text_delta", "text": part},
			}) {
				return false
			}
			sent.WriteString(part)
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
			finishLog(nil, r.Context().Err())
			return
		case <-ticker.C:
			if !send("ping", map[string]any{"type": "ping"}) {
				return
			}
		case ev := <-deltaCh:
			if ev.kind == "step" {
				// 新的一步：先把当前思考/正文块收尾，下一步会开新的块。
				closeBlock()
				continue
			}
			if ev.kind == "reasoning" {
				if !emitThinking(ev.text) {
					return
				}
				continue
			}
			if !emitText(ev.text) {
				return
			}
		case out := <-outCh:
			finishLog(out.comp, out.err)
			if out.err != nil {
				send("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": out.err.Error()}})
				return
			}
			content := out.comp.Content
			switch {
			case strings.HasPrefix(content, sent.String()):
				if rest := content[len(sent.String()):]; rest != "" {
					if !emitText(rest) {
						return
					}
				}
			case sent.Len() == 0:
				if !emitText(content) {
					return
				}
			default:
				common := 0
				cur := sent.String()
				minLen := len(content)
				if len(cur) < minLen {
					minLen = len(cur)
				}
				for common < minLen && content[common] == cur[common] {
					common++
				}
				if rest := content[common:]; rest != "" {
					if !emitText(rest) {
						return
					}
				}
			}
			closeBlock()
			index := nextIndex
			for _, c := range out.comp.Calls {
				send("content_block_start", map[string]any{
					"type": "content_block_start", "index": index,
					"content_block": map[string]any{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": map[string]any{}},
				})
				send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": c.Function.Arguments},
				})
				send("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
				index++
			}
			send("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": stopReason(out.comp), "stop_sequence": nil},
				"usage": anthropicUsage(out.comp.Usage.InputTokens, out.comp.Usage.OutputTokens,
					out.comp.Usage.CacheReadTokens, out.comp.Usage.CacheWriteTokens),
			})
			send("message_stop", map[string]any{"type": "message_stop"})
			return
		}
	}

}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
}
