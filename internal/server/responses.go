package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/haohaomin/capy2api/internal/store"
)

// --------------------------------------------------------------- OpenAI Responses

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream"`
	Tools           []toolDef       `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning       *reasoningOpt   `json:"reasoning,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
}

type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// responsesToChat 把 /v1/responses 的 input 转成 chatMessage。
func responsesToChat(req *responsesRequest) ([]chatMessage, error) {
	var out []chatMessage
	if strings.TrimSpace(req.Instructions) != "" {
		raw, _ := json.Marshal(req.Instructions)
		out = append(out, chatMessage{Role: "system", Content: raw})
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return out, nil
	}
	var text string
	if err := json.Unmarshal(req.Input, &text); err == nil {
		raw, _ := json.Marshal(text)
		out = append(out, chatMessage{Role: "user", Content: raw})
		return out, nil
	}
	var items []responsesItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, fmt.Errorf("input 只能是字符串或消息数组")
	}
	for _, item := range items {
		if item.Role == "" && item.Type != "" && item.Type != "message" {
			continue
		}
		role := item.Role
		if role == "" {
			role = "user"
		}
		// 保留原始内容数组，交给 collectImages 抽图。
		if trimmed := strings.TrimSpace(string(item.Content)); strings.HasPrefix(trimmed, "[") {
			out = append(out, chatMessage{Role: role, Content: item.Content})
			continue
		}
		content, err := renderContent(item.Content)
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(content)
		out = append(out, chatMessage{Role: role, Content: raw})
	}
	return out, nil
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := decodeJSON(r, &req); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeAPIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	messages, err := responsesToChat(&req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	messages, images, err := collectImages(messages)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	chatReq := &chatRequest{
		Model:      req.Model,
		Messages:   messages,
		Tools:      req.Tools,
		ToolChoice: req.ToolChoice,
		Stream:     req.Stream,
		Reasoning:  req.Reasoning,
	}
	acc, err := s.store.Next()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error(), "no_account")
		return
	}
	models, err := s.catalog(r.Context(), acc, false)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "拉取模型目录失败: "+err.Error(), "upstream_error")
		return
	}
	model, err := resolveModel(chatReq.Model, models)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	choice, err := toolChoice(chatReq.ToolChoice, len(chatReq.Tools) > 0)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	reasoning := pickReasoning(model, firstNonEmpty(effortOf(chatReq.Reasoning), ""), s.cfg().Capy.Reasoning)
	nonce := ""
	var prompt string
	if len(chatReq.Tools) > 0 {
		nonce = shortID()
		prompt = buildToolPrompt(nonce, choice, true, chatReq.Tools, chatReq.Messages)
	} else {
		prompt, err = buildPlainPrompt(chatReq.Messages)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
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
		Tools:       len(chatReq.Tools),
		Client:      clientLabel(r),
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg().Capy.TurnTimeoutSec+40)*time.Second)
	defer cancel()
	files, ferr := s.uploadImages(r.Context(), acc, images)
	if ferr != nil {
		writeAPIError(w, http.StatusBadRequest, "图片处理失败: "+ferr.Error(), "invalid_image")
		return
	}
	if len(images) > 0 {
		prompt += fmt.Sprintf("\n\n（用户附带了 %d 张图片，已作为附件随消息上传，请结合图片内容回答。）", len(images))
	}
	comp, cerr := s.complete(ctx, acc, model, reasoning, prompt, nonce, choice, chatReq.Tools, true, files, "", false, false, nil, nil, nil, nil)
	entry.DurationMS = time.Since(start).Milliseconds()
	noteRequest(model.ID, cerr == nil, req.Stream, usageOf(comp), time.Since(start), resultOf(comp))
	recordHealth(cerr == nil, time.Since(start))
	if cerr != nil {
		entry.Error = cerr.Error()
		s.log.Push(entry)
		s.writeUpstreamError(w, cerr)
		return
	}
	entry.Status = "ok"
	entry.JamID = comp.Result.JamID
	entry.PromptTokens = comp.Usage.InputTokens
	entry.OutputTokens = comp.Usage.OutputTokens
	entry.CacheTokens = comp.Usage.CacheReadTokens
	s.log.Push(entry)

	respID := "resp_" + shortID()
	messageID := "msg_" + shortID()
	output := []map[string]any{{
		"id":     messageID,
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []map[string]any{{
			"type": "output_text", "text": comp.Content, "annotations": []any{},
		}},
	}}
	for _, c := range comp.Calls {
		var input any = map[string]any{}
		_ = json.Unmarshal([]byte(c.Function.Arguments), &input)
		output = append(output, map[string]any{
			"id": "fc_" + shortID(), "type": "function_call", "status": "completed",
			"call_id": c.ID, "name": c.Function.Name, "arguments": c.Function.Arguments, "input": input,
		})
	}
	body := map[string]any{
		"id":         respID,
		"object":     "response",
		"created_at": start.Unix(),
		"status":     "completed",
		"model":      model.ID,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":          comp.Usage.InputTokens,
			"output_tokens":         comp.Usage.OutputTokens,
			"total_tokens":          comp.Usage.InputTokens + comp.Usage.OutputTokens,
			"output_tokens_details": map[string]any{"reasoning_tokens": comp.Usage.ReasoningTokens},
		},
		"capy": comp.Trace,
	}

	if !req.Stream {
		writeJSON(w, http.StatusOK, body)
		return
	}
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
	send("response.created", map[string]any{"type": "response.created", "response": body})
	send("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	})
	for _, piece := range chunkText(comp.Content, s.cfg().Capy.Typewriter) {
		send("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageID, "output_index": 0,
			"content_index": 0, "delta": piece,
		})
	}
	send("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": messageID, "output_index": 0,
		"content_index": 0, "text": comp.Content,
	})
	send("response.completed", map[string]any{"type": "response.completed", "response": body})
}

func effortOf(r *reasoningOpt) string {
	if r == nil {
		return ""
	}
	return r.Effort
}
