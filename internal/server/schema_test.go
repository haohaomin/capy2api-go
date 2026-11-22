package server

import "testing"

func TestValidateToolArguments(t *testing.T) {
	schema := []byte(`{
		"type":"object",
		"properties":{"path":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1}},
		"required":["path"],
		"additionalProperties":false
	}`)
	if err := validateToolArguments(map[string]any{"path": "a.go", "limit": float64(10)}, schema); err != nil {
		t.Fatalf("合法参数被拒: %v", err)
	}
	if err := validateToolArguments(map[string]any{"limit": float64(10)}, schema); err == nil {
		t.Fatal("缺少必填参数应被拒")
	}
	if err := validateToolArguments(map[string]any{"path": ""}, schema); err == nil {
		t.Fatal("minLength 违反应被拒")
	}
	if err := validateToolArguments(map[string]any{"path": "a", "extra": 1}, schema); err == nil {
		t.Fatal("additionalProperties=false 应拒绝多余字段")
	}
	if err := validateToolArguments(map[string]any{"path": "a", "limit": float64(0)}, schema); err == nil {
		t.Fatal("minimum 违反应被拒")
	}
}

func TestParseToolDecisionSchema(t *testing.T) {
	tools := []toolDef{{Type: "function"}}
	tools[0].Function.Name = "get_weather"
	tools[0].Function.Parameters = []byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`)
	nonce := "abc"
	bad := `{"protocol":"capy2api.tools.v1","request_id":"abc","kind":"tool_calls","calls":[{"name":"get_weather","arguments":{}}]}`
	if _, _, _, err := parseToolDecision(bad, nonce, "auto", tools, true); err == nil {
		t.Fatal("缺少必填参数的工具决策应被拒绝")
	}
	good := `{"protocol":"capy2api.tools.v1","request_id":"abc","kind":"tool_calls","calls":[{"name":"get_weather","arguments":{"city":"北京"}}]}`
	_, calls, reason, err := parseToolDecision(good, nonce, "auto", tools, true)
	if err != nil || len(calls) != 1 || reason != "tool_calls" {
		t.Fatalf("合法决策应通过: err=%v calls=%d reason=%s", err, len(calls), reason)
	}
}
