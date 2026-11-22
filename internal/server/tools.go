package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

const toolProtocol = "capy2api.tools.v1"

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type toolCallOut struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDecision struct {
	Protocol  string `json:"protocol"`
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	Calls     []struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"calls"`
}

// toolChoice 归一化 tool_choice 参数。
func toolChoice(raw json.RawMessage, hasTools bool) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if hasTools {
			return "auto", nil
		}
		return "none", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			if s == "required" && !hasTools {
				return "", fmt.Errorf("tool_choice=required 需要先声明 tools")
			}
			return s, nil
		}
		return "", fmt.Errorf("不支持的 tool_choice: %s", s)
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
		return "", fmt.Errorf("tool_choice 只支持 \"auto\"/\"none\"/\"required\" 或 {type:function,...}")
	}
	return obj.Function.Name, nil
}

// buildToolPrompt 生成「模型只输出 JSON 决策」的提示词。
func buildToolPrompt(nonce, choice string, parallel bool, tools []toolDef, messages []chatMessage) string {
	type toolView struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	}
	views := make([]toolView, 0, len(tools))
	for _, t := range tools {
		views = append(views, toolView{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	payload := map[string]any{
		"request_id":          nonce,
		"tool_choice":         choice,
		"parallel_tool_calls": parallel,
		"tools":               views,
		"messages":            messagesForPrompt(messages),
	}
	body, _ := json.Marshal(payload)

	var b strings.Builder
	b.WriteString("你是本地编程 agent 的推理组件。下面 JSON 是真实对话与客户端工具定义；工具由客户端执行，你只负责决策。\n")
	b.WriteString("【输出格式】如果回答里包含代码/HTML/CSS/JS，必须用 ```语言 代码块包裹，不要把整段代码当纯文本输出。\n")
	b.WriteString("【写文件】如果任务是把内容写入/修改文件，必须调用客户端的 write/edit 工具，不要把整份文件内容放进最终回答；最终回答只给一句摘要。\n")
	b.WriteString("禁止调用任何平台/云端工具（shell、read、write、edit、bash、MCP、浏览、任务等）：你没有任何执行权限，这些工具也不存在。只做一件事：输出一个 JSON 决策。\n")
	b.WriteString("硬性规则：\n")
	b.WriteString("1. 只输出一个 JSON 对象，不要 markdown 代码块、不要任何解释文字。\n")
	b.WriteString("2. 需要调用工具时输出：{\"protocol\":\"" + toolProtocol + "\",\"request_id\":\"" + nonce + "\",\"kind\":\"tool_calls\",\"calls\":[{\"name\":\"<工具名>\",\"arguments\":{...}}]}\n")
	b.WriteString("3. 直接回答时输出：{\"protocol\":\"" + toolProtocol + "\",\"request_id\":\"" + nonce + "\",\"kind\":\"final\",\"content\":\"<回答>\"}\n")
	b.WriteString("4. arguments 必须符合该工具的 JSON Schema；工具名必须来自 tools 列表。\n")
	b.WriteString("5. tool_choice=none 时禁止调用工具；required 时必须调用；指定函数名时只能调用该函数。\n")
	b.WriteString("6. parallel_tool_calls=false 时最多一个 call。不要臆造工具结果，不要自己执行命令。\n\n")
	b.WriteString("客户端声明的工具如下（这是你唯一可以调用的工具集合，参数必须符合各自的 JSON Schema）：\n")
	b.Write(body)
	b.WriteString("\n\n再次强调：本轮 request_id 是 " + nonce + "，只输出一个 JSON 对象。")
	return b.String()
}

// parseToolDecision 严格解析模型给出的决策。
func parseToolDecision(text, nonce, choice string, tools []toolDef, parallel bool) (string, []toolCallOut, string, error) {
	raw := strings.TrimSpace(text)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
		raw = strings.TrimSpace(raw)
	}
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Function.Name] = true
	}
	// 容错：模型把 JSON 包在自然语言里时，取第一个 { 到最后一个 } 之间。
	candidate := raw
	if !strings.HasPrefix(candidate, "{") {
		if i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); i >= 0 && j > i {
			candidate = raw[i : j+1]
		}
	}
	var dec toolDecision
	decoder := json.NewDecoder(strings.NewReader(candidate))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&dec); err != nil {
		if choice == "auto" || choice == "none" {
			if looksLikeProse(raw) {
				return raw, nil, "stop", nil
			}
		}
		// 流式 JSON 被上游截断（unexpected EOF）时，尽量把已经写出的 content 抢救出来，
		// 而不是整轮报错。
		ex := &finalTextExtractor{}
		var salvaged string
		for _, piece := range ex.Feed(raw) {
			salvaged += piece
		}
		if strings.TrimSpace(salvaged) != "" {
			log.Printf("WARN 工具决策 JSON 不完整，已从截断内容中恢复 %d 字", len([]rune(salvaged)))
			return salvaged, nil, "stop", nil
		}
		return "", nil, "", fmt.Errorf("模型工具决策不是合法 JSON（%v）", err)
	}
	if dec.Protocol != toolProtocol {
		return "", nil, "", fmt.Errorf("工具协议不匹配：%q", dec.Protocol)
	}
	if dec.RequestID != "" && dec.RequestID != nonce {
		// 线程复用时模型可能沿用上一轮的 request_id；这不该让整轮失败。
		// 线程复用时模型会沿用旧 request_id，属于正常现象，只记调试信息。
	}
	switch dec.Kind {
	case "final":
		if choice != "auto" && choice != "none" {
			return "", nil, "", fmt.Errorf("tool_choice=%s 要求调用工具", choice)
		}
		if strings.TrimSpace(dec.Content) == "" {
			return "", nil, "", fmt.Errorf("final 决策的 content 为空")
		}
		if len(dec.Calls) != 0 {
			return "", nil, "", fmt.Errorf("final 决策不应带 calls")
		}
		return dec.Content, nil, "stop", nil
	case "tool_calls":
		if choice == "none" {
			return "", nil, "", fmt.Errorf("tool_choice=none 时禁止调用工具")
		}
		if len(dec.Calls) == 0 {
			return "", nil, "", fmt.Errorf("calls 为空")
		}
		limit := 16
		if !parallel {
			limit = 1
		}
		if len(dec.Calls) > limit {
			return "", nil, "", fmt.Errorf("calls 数量超过上限 %d", limit)
		}
		out := make([]toolCallOut, 0, len(dec.Calls))
		for _, c := range dec.Calls {
			if !known[c.Name] {
				return "", nil, "", fmt.Errorf("模型调用了未声明的工具 %q", c.Name)
			}
			if choice != "auto" && choice != "required" && choice != c.Name {
				return "", nil, "", fmt.Errorf("tool_choice=%s 与调用的工具 %q 不一致", choice, c.Name)
			}
			if c.Arguments == nil {
				c.Arguments = map[string]any{}
			}
			if err := validateToolArguments(c.Arguments, parametersOf(tools, c.Name)); err != nil {
				return "", nil, "", fmt.Errorf("工具参数不符合 JSON Schema（%s）：%v", c.Name, err)
			}
			args, err := json.Marshal(c.Arguments)
			if err != nil {
				return "", nil, "", fmt.Errorf("工具参数序列化失败: %w", err)
			}
			call := toolCallOut{ID: "call_" + shortID(), Type: "function"}
			call.Function.Name = c.Name
			call.Function.Arguments = string(args)
			out = append(out, call)
		}
		return "", out, "tool_calls", nil
	default:
		return "", nil, "", fmt.Errorf("未知 kind: %q", dec.Kind)
	}
}

// parametersOf 取声明工具的 JSON Schema。
func parametersOf(tools []toolDef, name string) json.RawMessage {
	for _, t := range tools {
		if t.Function.Name == name {
			return t.Function.Parameters
		}
	}
	return nil
}

// looksLikeProse 判断文本是否是普通回答（而不是坏掉的工具 JSON）。
func looksLikeProse(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") || strings.HasPrefix(t, "```") {
		return false
	}
	low := strings.ToLower(t)
	for _, bad := range []string{"tool_calls", "request_id", "capy2api.tools", "\"calls\""} {
		if strings.Contains(low, bad) {
			return false
		}
	}
	return true
}

// buildDeltaPrompt 把「上次之后新增的消息」渲染成一段文本，交给已复用的线程。
//
// 工具调用是多轮的：客户端第二次请求会带上 assistant.tool_calls 和 role=tool 的结果。
// 如果只发最后一条 user 文本，工具结果就永远到不了模型（表现为「工具结果为空」）。
func buildDeltaPrompt(messages []chatMessage, sent int) string {
	if sent < 0 || sent > len(messages) {
		sent = 0
	}
	var b strings.Builder
	b.WriteString("客户端新增了以下消息（补充在你上一轮回答之后）：\n")
	for _, m := range messages[sent:] {
		text, _ := renderContent(m.Content)
		switch m.Role {
		case "tool":
			b.WriteString("- [tool 结果] tool_call_id=" + m.ToolCallID + "：" + text + "\n")
		case "assistant":
			// 助手自己的历史正文不需要原样回灌（常常是一整份文件），截断即可，
			// 否则 prompt 会被上一轮的 19KB HTML 撑爆。
			if len([]rune(text)) > 800 {
				text = string([]rune(text)[:800]) + "…（已截断）"
			}
			if len(m.ToolCalls) > 0 {
				names := make([]string, 0, len(m.ToolCalls))
				for _, c := range m.ToolCalls {
					names = append(names, c.Function.Name)
				}
				b.WriteString("- [assistant 发起了工具调用] " + strings.Join(names, ", ") + "\n")
			}
			if strings.TrimSpace(text) != "" {
				b.WriteString("- [assistant] " + text + "\n")
			}
		case "user":
			b.WriteString("- [user] " + text + "\n")
		case "system":
			b.WriteString("- [system] " + text + "\n")
		}
	}
	b.WriteString("\n请基于这些新消息继续：该调用工具就调用，能回答就直接回答。")
	return b.String()
}
