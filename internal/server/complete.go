package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/store"
)

// completion 是一次完整的回答（已解析工具决策）。
type completion struct {
	Content string
	Calls   []toolCallOut
	Reason  string
	Usage   capy.Usage
	Trace   map[string]any
	Result  *capy.TurnResult
}

// usageOf 安全取 usage（comp 为空时返回零值）。
func usageOf(c *completion) capy.Usage {
	if c == nil {
		return capy.Usage{}
	}
	return c.Usage
}

// resultOf 安全取上游回合结果（comp 为空时返回 nil）。
func resultOf(c *completion) *capy.TurnResult {
	if c == nil {
		return nil
	}
	return c.Result
}

// complete 跑一次上游回合并把模型输出解析成 OpenAI 语义。
func (s *Server) complete(ctx context.Context, acc store.Account, model capy.Model, reasoning, prompt, nonce, choice string, tools []toolDef, parallel bool, files []capy.FileRef, existingJam string, keepThread, promptDelivered bool, onDelta, onText, onReason func(string), onStep func()) (*completion, error) {
	// 新会话且伪造设备在线：直接建一条带 prompt 的根线程（Anthropic/Responses 路径不会走 chat.go 的池逻辑）。
	if existingJam == "" && s.daemon() != nil && s.daemon().Status()["online"] == true &&
		strings.ToLower(firstNonEmpty(acc.Mode, s.cfg().Capy.Mode, "thread")) == "thread" {
		if jam, jerr := s.takeRootThreadForPrompt(ctx, acc, model, reasoning, prompt, files); jerr == nil {
			existingJam = jam
			promptDelivered = true
		}
	}
	res, err := s.runTurn(ctx, acc, model, reasoning, prompt, "capy2api", files, existingJam, keepThread, promptDelivered, onDelta, onText, onReason, onStep)
	if err != nil {
		return nil, err
	}
	out := &completion{Usage: res.Usage, Result: res}
	if len(tools) > 0 {
		content, calls, reason, perr := parseToolDecision(res.Text, nonce, choice, tools, parallel)
		if perr != nil {
			return nil, fmt.Errorf("模型工具决策被拒绝：%w", perr)
		}
		out.Content, out.Calls, out.Reason = content, calls, reason
	} else {
		out.Content, out.Reason = res.Text, "stop"
	}
	out.Trace = s.traceBlock(res, reasoning)
	s.recordContext(model.ID, res)
	return out, nil
}
