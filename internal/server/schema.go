// schema.go 工具参数的 JSON Schema 子集校验（$ref/type/enum/const/allOf/anyOf/oneOf/
// required/properties/items/pattern/min/max/additionalProperties）。
//
// 客户端仍然要做完整校验；这里只是保证「模型决策 → 客户端执行」这条链上，
// 网关不会把明显不合法的参数当成工具调用发出去。
package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

func validateToolArguments(args map[string]any, schemaRaw json.RawMessage) error {
	if len(schemaRaw) == 0 {
		return nil
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return nil // schema 本身不合法时不阻塞，交给客户端
	}
	return validateSchema(args, schema, schema, 0)
}

func validateSchema(value any, schema, root map[string]any, depth int) error {
	if depth > 64 {
		return fmt.Errorf("参数嵌套过深")
	}
	if schema == nil {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			return nil
		}
		target := any(root)
		for _, part := range strings.Split(ref[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			m, ok := target.(map[string]any)
			if !ok {
				return nil
			}
			target = m[part]
		}
		if m, ok := target.(map[string]any); ok {
			return validateSchema(value, m, root, depth+1)
		}
		return nil
	}
	if want, ok := schema["type"]; ok {
		if !typeMatches(value, want) {
			return fmt.Errorf("参数类型不匹配（期望 %v）", want)
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("参数不在枚举范围内")
		}
	}
	if c, ok := schema["const"]; ok && fmt.Sprint(c) != fmt.Sprint(value) {
		return fmt.Errorf("参数不等于 const")
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		list, ok := schema[key].([]any)
		if !ok {
			continue
		}
		matches := 0
		for _, sub := range list {
			if m, ok := sub.(map[string]any); ok && validateSchema(value, m, root, depth+1) == nil {
				matches++
			}
		}
		if key == "allOf" {
			for _, sub := range list {
				if m, ok := sub.(map[string]any); ok {
					if err := validateSchema(value, m, root, depth+1); err != nil {
						return err
					}
				}
			}
			continue
		}
		if matches == 0 || (key == "oneOf" && matches != 1) {
			return fmt.Errorf("参数不满足 %s 约束", key)
		}
	}
	if obj, ok := value.(map[string]any); ok {
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, exists := obj[name]; !exists {
					return fmt.Errorf("缺少必填参数 %q", name)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for k, v := range obj {
			if sub, ok := props[k].(map[string]any); ok {
				if err := validateSchema(v, sub, root, depth+1); err != nil {
					return err
				}
				continue
			}
			if schema["additionalProperties"] == false {
				return fmt.Errorf("出现未声明参数 %q", k)
			}
			if sub, ok := schema["additionalProperties"].(map[string]any); ok {
				if err := validateSchema(v, sub, root, depth+1); err != nil {
					return err
				}
			}
		}
	}
	if arr, ok := value.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for _, v := range arr {
				if err := validateSchema(v, items, root, depth+1); err != nil {
					return err
				}
			}
		}
	}
	if s, ok := value.(string); ok {
		if p, ok := schema["pattern"].(string); ok && p != "" {
			if re, err := regexp.Compile(p); err == nil && !re.MatchString(s) {
				return fmt.Errorf("参数不匹配 pattern")
			}
		}
		if n, ok := numField(schema["minLength"]); ok && float64(len([]rune(s))) < n {
			return fmt.Errorf("参数长度小于 minLength")
		}
		if n, ok := numField(schema["maxLength"]); ok && float64(len([]rune(s))) > n {
			return fmt.Errorf("参数长度超过 maxLength")
		}
	}
	if f, ok := toFloat(value); ok {
		if n, ok := numField(schema["minimum"]); ok && f < n {
			return fmt.Errorf("参数小于 minimum")
		}
		if n, ok := numField(schema["maximum"]); ok && f > n {
			return fmt.Errorf("参数大于 maximum")
		}
	}
	return nil
}

func typeMatches(v any, want any) bool {
	check := func(t string) bool {
		switch t {
		case "object":
			_, ok := v.(map[string]any)
			return ok
		case "array":
			_, ok := v.([]any)
			return ok
		case "string":
			_, ok := v.(string)
			return ok
		case "boolean":
			_, ok := v.(bool)
			return ok
		case "integer":
			f, ok := toFloat(v)
			return ok && f == float64(int64(f))
		case "number":
			_, ok := toFloat(v)
			return ok
		case "null":
			return v == nil
		}
		return false
	}
	switch w := want.(type) {
	case string:
		return check(w)
	case []any:
		for _, item := range w {
			if s, ok := item.(string); ok && check(s) {
				return true
			}
		}
		return false
	}
	return true
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func numField(v any) (float64, bool) { return toFloat(v) }
