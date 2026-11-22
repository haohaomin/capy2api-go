package capy

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// RealtimeURL 是 Capy 的实时事件网关（网页端也在用同一条通道）。
const RealtimeURL = "wss://realtime-production.cloud.capy.ai/realtime"

// Realtime 订阅单条线程的增量事件：首字时间、逐字文本、每步 usage。
//
// REST /events 只在 assistant committed 时给完整结果，拿不到「首字」和生成速度；
// 实时通道给的是 llm-event 增量（text-delta / tool-input-delta / step-finish）。
type Realtime struct {
	conn *websocket.Conn

	mu         sync.Mutex
	firstAt    time.Time
	lastAt     time.Time
	chars      int
	text       strings.Builder
	streamText map[string]*strings.Builder
	toolArgs   map[string]*strings.Builder
	usage      Usage
	onGrow     func(string) // 文本增长时回调（用于真流式下发）
	onText     func(string) // message_user 工具里的可见文本增量（工具模式的最终答案）
	onReason   func(string) // 思考过程增量（reasoning-delta）
	onStep     func()       // 新的步骤开始（用于分开多段思考）
	onTool     func(string) // 工具调用开始（用于切断思考块）
	started    time.Time    // 连接建立时间（调试用）
	debug      bool
	sawStep    bool
	closed     bool
}

// DialRealtime 连接实时网关并完成 auth + subscribe。
func DialRealtime(ctx context.Context, token, jamID string) (*Realtime, error) {
	conn, err := websocket.Dial(RealtimeURL, "", "https://capy.ai")
	if err != nil {
		return nil, err
	}
	rt := &Realtime{
		conn:       conn,
		streamText: map[string]*strings.Builder{},
		toolArgs:   map[string]*strings.Builder{},
		started:    time.Now(),
		debug:      os.Getenv("CAPY2API_RT_LOG") == "1",
	}
	send := func(v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetWriteDeadline(dl)
		} else {
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		}
		return websocket.Message.Send(conn, string(raw))
	}
	if err := send(map[string]any{"type": "auth", "token": token}); err != nil {
		conn.Close()
		return nil, err
	}
	if err := send(map[string]any{"type": "subscribe", "scopes": []string{"jam:" + jamID}}); err != nil {
		conn.Close()
		return nil, err
	}
	return rt, nil
}

// Close 关闭连接。
func (r *Realtime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	_ = r.conn.Close()
}

// Snapshot 是实时通道的观测结果。
type Snapshot struct {
	FirstAt    time.Time
	LastAt     time.Time
	Chars      int
	Text       string
	Usage      Usage
	DeltaCount int
	HasUsage   bool
}

// Snapshot 读取当前累计状态。
func (r *Realtime) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{
		FirstAt: r.firstAt, LastAt: r.lastAt, Chars: r.chars,
		Text: r.text.String(), Usage: r.usage, DeltaCount: r.chars,
	}
}

// Loop 阻塞读取事件直到连接关闭或 ctx 结束。
func (r *Realtime) Loop(ctx context.Context) {
	if r == nil {
		return
	}
	go func() {
		<-ctx.Done()
		r.Close()
	}()
	for {
		var raw string
		_ = r.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		if err := websocket.Message.Receive(r.conn, &raw); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		r.handle(raw, time.Now())
	}
}

func (r *Realtime) handle(raw string, at time.Time) {
	var frame map[string]any
	if json.Unmarshal([]byte(raw), &frame) != nil {
		return
	}
	if r.debug {
		typ, _ := frame["type"].(string)
		delta, _ := frame["delta"].(map[string]any)
		k := ""
		if delta != nil {
			if v, ok := delta["type"].(string); ok {
				k = v
			}
			if ev, ok := delta["event"].(map[string]any); ok {
				if et, ok := ev["type"].(string); ok {
					k += "/" + et
				}
			}
		}
		log.Printf("RT %s/%s at=%dms since_start=%dms", typ, k, at.UnixMilli(), at.Sub(r.started).Milliseconds())
	}
	if typ, _ := frame["type"].(string); typ != "delta" {
		return
	}
	delta, _ := frame["delta"].(map[string]any)
	if delta == nil {
		return
	}
	if dt, _ := delta["type"].(string); dt != "llm-event" {
		return
	}
	event, _ := delta["event"].(map[string]any)
	if event == nil {
		return
	}
	kind, _ := event["type"].(string)
	id, _ := event["id"].(string)
	switch kind {
	case "reasoning-delta":
		text, _ := event["text"].(string)
		if text != "" {
			r.mark(at)
			if cb := r.onReason; cb != nil {
				cb(text)
			}
		}
	case "step-start":
		if cb := r.onStep; cb != nil {
			cb()
		}
		r.mu.Lock()
		r.sawStep = false
		r.mu.Unlock()
		r.mark(at)
	case "text-delta":
		text, _ := event["text"].(string)
		if text == "" {
			return
		}
		r.mark(at)
		r.mu.Lock()
		b := r.streamText[id]
		if b == nil {
			b = &strings.Builder{}
			r.streamText[id] = b
		}
		b.WriteString(text)
		r.text.WriteString(text)
		r.chars += len([]rune(text))
		cb := r.onGrow
		r.mu.Unlock()
		if cb != nil {
			cb(text)
		}
	case "tool-input-start":
		r.mu.Lock()
		r.toolArgs[id] = &strings.Builder{}
		r.mu.Unlock()
	case "tool-input-delta":
		text, _ := event["text"].(string)
		if text == "" {
			return
		}
		r.mu.Lock()
		b := r.toolArgs[id]
		if b == nil {
			b = &strings.Builder{}
			r.toolArgs[id] = b
		}
		b.WriteString(text)
		visible, _ := jsonStringFieldPrefix(b.String(), "text")
		r.mu.Unlock()
		if visible != "" {
			r.mark(at)
			r.mu.Lock()
			deltaTxt := ""
			if n := len([]rune(visible)); n > r.chars {
				deltaTxt = string([]rune(visible)[r.chars:])
				r.text.WriteString(deltaTxt)
				r.chars = n
			}
			cbText := r.onText
			r.mu.Unlock()
			if cbText != nil && deltaTxt != "" {
				cbText(deltaTxt)
			}
		}
	case "tool-call":
		r.mark(at)
		if cb := r.onTool; cb != nil {
			if name, _ := event["name"].(string); name != "" && name != "message_user" {
				cb(name)
			}
		}
		if input, ok := event["input"].(map[string]any); ok {
			if text, ok := input["text"].(string); ok && text != "" {
				r.mu.Lock()
				// tool-call 是最终入参：只补足没被增量覆盖的部分。
				deltaTxt := ""
				if n := len([]rune(text)); n > r.chars {
					deltaTxt = string([]rune(text)[r.chars:])
					r.text.WriteString(deltaTxt)
					r.chars = n
				}
				cb := r.onGrow
				r.mu.Unlock()
				if cb != nil && deltaTxt != "" {
					cb(deltaTxt)
				}
			}
		}
	case "step-finish", "finish":
		// step-finish 与 finish 会带同一份 usage：只计一次，避免 inputTokens 翻倍。
		if u, ok := event["usage"].(map[string]any); ok {
			r.mu.Lock()
			if kind == "step-finish" {
				r.sawStep = true
				r.usage.Add(usageFromMap(u))
			} else if !r.sawStep {
				r.usage.Add(usageFromMap(u))
			}
			r.mu.Unlock()
		}
		r.mark(at)
	}
}

func (r *Realtime) mark(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstAt.IsZero() {
		r.firstAt = at
	}
	r.lastAt = at
}

// usageFromMap 兼容实时事件与 REST 事件的字段命名差异。
func usageFromMap(m map[string]any) Usage {
	num := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := m[k].(float64); ok {
				return int(v)
			}
		}
		return 0
	}
	return Usage{
		InputTokens:          num("inputTokens", "input_tokens", "promptTokens", "prompt_tokens"),
		OutputTokens:         num("outputTokens", "output_tokens", "completionTokens", "completion_tokens"),
		CacheReadTokens:      num("cacheReadTokens", "cacheReadInputTokens", "cache_read_tokens"),
		CacheWriteTokens:     num("cacheWriteTokens", "cacheWriteInputTokens", "cache_write_tokens"),
		ReasoningTokens:      num("reasoningTokens", "reasoning_tokens"),
		NonCachedInputTokens: num("nonCachedInputTokens", "non_cached_input_tokens"),
		TotalTokens:          num("totalTokens", "total_tokens"),
	}
}

// jsonStringFieldPrefix 从一个可能还没写完的 JSON 对象里取出根级字符串字段的当前值。
//
// 用于 message_user 的 tool-input-delta：模型把最终答复作为 JSON 的 text 字段
// 一片片吐出来，这里把已到达的部分解码出来，才能算出「首字」并把增量交给客户端。
func jsonStringFieldPrefix(buf, field string) (string, bool) {
	pos := 0
	skipSpace := func() {
		for pos < len(buf) && (buf[pos] == ' ' || buf[pos] == '\n' || buf[pos] == '\r' || buf[pos] == '\t') {
			pos++
		}
	}
	skipSpace()
	if pos >= len(buf) || buf[pos] != '{' {
		return "", false
	}
	pos++
	for {
		skipSpace()
		if pos >= len(buf) {
			return "", false
		}
		if buf[pos] == '}' {
			return "", false
		}
		if buf[pos] != '"' {
			return "", false
		}
		key, next, ok := jsonStringAt(buf, pos)
		pos = next
		if !ok {
			return "", false
		}
		skipSpace()
		if pos >= len(buf) || buf[pos] != ':' {
			return "", false
		}
		pos++
		skipSpace()
		if key == field {
			if pos >= len(buf) || buf[pos] != '"' {
				return "", false
			}
			value, _, _ := jsonStringAt(buf, pos)
			return value, true
		}
		// 跳过该字段的值：只支持字符串与简单标量。
		if pos < len(buf) && buf[pos] == '"' {
			_, next, _ := jsonStringAt(buf, pos)
			pos = next
		} else {
			for pos < len(buf) && buf[pos] != ',' && buf[pos] != '}' {
				pos++
			}
		}
		skipSpace()
		if pos < len(buf) && buf[pos] == ',' {
			pos++
			continue
		}
		return "", false
	}
}

// jsonStringAt 解码 buf[pos] 处的 JSON 字符串（允许未收尾）。
func jsonStringAt(buf string, pos int) (value string, next int, complete bool) {
	if pos >= len(buf) || buf[pos] != '"' {
		return "", pos, false
	}
	pos++
	var b strings.Builder
	for pos < len(buf) {
		c := buf[pos]
		if c == '"' {
			return b.String(), pos + 1, true
		}
		if c == '\\' {
			if pos+1 >= len(buf) {
				return b.String(), pos, false
			}
			esc := buf[pos+1]
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
				pos += 2
			case 'n':
				b.WriteByte('\n')
				pos += 2
			case 't':
				b.WriteByte('\t')
				pos += 2
			case 'r':
				b.WriteByte('\r')
				pos += 2
			case 'b':
				b.WriteByte('\b')
				pos += 2
			case 'f':
				b.WriteByte('\f')
				pos += 2
			case 'u':
				if pos+6 > len(buf) {
					return b.String(), pos, false
				}
				var r rune
				for i := 2; i < 6; i++ {
					r = r*16 + rune(hexVal(buf[pos+i]))
				}
				b.WriteRune(r)
				pos += 6
			default:
				b.WriteByte(esc)
				pos += 2
			}
			continue
		}
		b.WriteByte(c)
		pos++
	}
	return b.String(), pos, false
}

func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	default:
		return 0
	}
}
