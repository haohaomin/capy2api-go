package server

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// finalTextExtractor 从流式的「协议 JSON」里增量提取 "content" 字段的文本。
//
// 工具模式下模型输出的是一个 JSON 对象（kind=final/tool_calls）；直接把原始增量发给
// 客户端会泄漏协议，等到结束再发又会让首字延迟等于整个回合。这里只把 final 的
// content 字段边写边解出来，客户端就能看到正常的流式输出。
type finalTextExtractor struct {
	buf     strings.Builder
	scan    int  // 已扫描到的字节位置
	started bool // 已定位到 content 字段的起始引号
	done    bool // content 已闭合
}

// Feed 输入一段原始增量，返回可以下发给客户端的新文本。
func (e *finalTextExtractor) Feed(chunk string) []string {
	if e.done || chunk == "" {
		return nil
	}
	e.buf.WriteString(chunk)
	s := e.buf.String()

	if !e.started {
		idx := strings.Index(s[e.scan:], `"content"`)
		if idx < 0 {
			// 保留一点尾部，避免键名被切断
			if len(s) > 16 {
				e.scan = len(s) - 16
			}
			return nil
		}
		i := e.scan + idx + len(`"content"`)
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) || s[i] != ':' {
			e.scan = i
			return nil
		}
		i++
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) {
			e.scan = i
			return nil
		}
		if s[i] != '"' {
			// 不是字符串（比如 kind=tool_calls 没有 content），不再提取
			return nil
		}
		e.started = true
		e.scan = i + 1
	}

	var out []string
	var sb strings.Builder
	flush := func() {
		if sb.Len() > 0 {
			out = append(out, sb.String())
			sb.Reset()
		}
	}
	for e.scan < len(s) {
		c := s[e.scan]
		if c == '\\' {
			if e.scan+1 >= len(s) {
				break
			}
			switch s[e.scan+1] {
			case 'n':
				sb.WriteByte('\n')
				e.scan += 2
			case 't':
				sb.WriteByte('\t')
				e.scan += 2
			case 'r':
				sb.WriteByte('\r')
				e.scan += 2
			case '"':
				sb.WriteByte('"')
				e.scan += 2
			case '\\':
				sb.WriteByte('\\')
				e.scan += 2
			case '/':
				sb.WriteByte('/')
				e.scan += 2
			case 'u':
				if e.scan+6 > len(s) {
					flush()
					return out
				}
				v, err := strconv.ParseUint(s[e.scan+2:e.scan+6], 16, 32)
				if err != nil {
					e.scan += 2
					continue
				}
				r := rune(v)
				// 代理对
				if r >= 0xD800 && r <= 0xDBFF && e.scan+12 <= len(s) && s[e.scan+6] == '\\' && s[e.scan+7] == 'u' {
					if lo, err2 := strconv.ParseUint(s[e.scan+8:e.scan+12], 16, 32); err2 == nil {
						r = ((r - 0xD800) << 10) + (rune(lo) - 0xDC00) + 0x10000
						e.scan += 6
					}
				}
				sb.WriteRune(r)
				e.scan += 6
			default:
				e.scan += 2
			}
			continue
		}
		if c == '"' {
			e.scan++
			e.done = true
			flush()
			return out
		}
		r, size := utf8.DecodeRuneInString(s[e.scan:])
		if r == utf8.RuneError && size == 1 {
			// 半个多字节字符，等下一段
			break
		}
		sb.WriteRune(r)
		e.scan += size
	}
	flush()
	return out
}
