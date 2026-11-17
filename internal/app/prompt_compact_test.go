package app

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func compactPromptSchemas(t *testing.T, prompt string) json.RawMessage {
	t.Helper()
	_, tail, ok := strings.Cut(prompt, "Available tools:\n")
	if !ok {
		t.Fatal("missing tool definitions")
	}
	var raw json.RawMessage
	if err := json.NewDecoder(strings.NewReader(tail)).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPromptCompactSchemaSizeAndContract(t *testing.T) {
	var tools []map[string]interface{}
	var expected []map[string]interface{}
	for i := 0; i < 30; i++ {
		props := map[string]interface{}{}
		for j := 0; j < 12; j++ {
			props[fmt.Sprintf("field_%d", j)] = map[string]interface{}{"type": "string", "description": "keep  double spaces\nand newline", "enum": []string{"a b", "c"}}
		}
		fn := map[string]interface{}{"name": fmt.Sprintf("tool_%d", i), "description": "verbatim  description\nsecond line", "parameters": map[string]interface{}{"type": "object", "properties": props, "required": []string{"field_0"}, "additionalProperties": false}}
		expected = append(expected, fn)
		tools = append(tools, map[string]interface{}{"type": "function", "function": fn})
	}
	prompt := buildPrompt([]map[string]interface{}{{"role": "user", "content": "unchanged"}}, tools, nil)
	raw := compactPromptSchemas(t, prompt)
	pretty, _ := json.MarshalIndent(expected, "", "  ")
	var got, want interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(pretty, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("schema semantics changed")
	}
	if len(raw) >= len(pretty)*80/100 {
		t.Fatalf("insufficient syntactic reduction: compact=%d pretty=%d", len(raw), len(pretty))
	}
	if strings.Contains(string(raw), "\n") {
		t.Fatal("schema indentation retained")
	}
	t.Logf("synthetic 30-tool schema bytes: %d -> %d (%.1f%% saved); total prompt %d", len(pretty), len(raw), 100*float64(len(pretty)-len(raw))/float64(len(pretty)), len(prompt))
}

func TestPromptCompactPreservesRawContentAndArguments(t *testing.T) {
	system := "raw system  spaces\n\nDo not change IDs: item_123"
	user := "raw user  spaces\n```json\n{  \"a\": 1 }\n```"
	result := "{\n  \"integer\": 9007199254740993,\n  \"signed\": \"x  y\",\n  \"id\": \"call_123\"\n}"
	args := "{  \"path\": \"a  b\", \"id\": \"item_123\" }"
	messages := []map[string]interface{}{{"role": "system", "content": system}, {"role": "user", "content": user}, {"role": "assistant", "content": "", "tool_calls": []interface{}{map[string]interface{}{"id": "call_123", "function": map[string]interface{}{"name": "read", "arguments": args}}}}, {"role": "tool", "name": "read", "tool_call_id": "call_123", "content": result}}
	prompt := buildPrompt(messages, nil, nil)
	for _, raw := range []string{system, user, result, args} {
		if !strings.Contains(prompt, raw) {
			t.Fatalf("raw content changed: %q", raw)
		}
	}
	if got := formatToolResult("read", result); got != "[Tool result for read]: "+result {
		t.Fatal("JSON tool result rewritten")
	}
}

func TestPromptCompactToolChoiceContract(t *testing.T) {
	tools := []map[string]interface{}{{"type": "function", "function": map[string]interface{}{"name": "alpha", "description": "A", "parameters": map[string]interface{}{"type": "object"}}}, {"type": "function", "function": map[string]interface{}{"name": "beta", "description": "B", "parameters": map[string]interface{}{"type": "object"}}}}
	if p := buildPrompt(nil, tools, "none"); strings.Contains(p, "Available tools:") {
		t.Fatal("none injected tools")
	}
	p := buildPrompt(nil, tools, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "beta"}})
	var defs []map[string]interface{}
	if err := json.Unmarshal(compactPromptSchemas(t, p), &defs); err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0]["name"] != "beta" || !strings.Contains(p, `You MUST call the tool "beta"`) {
		t.Fatal("forced tool contract changed")
	}
	if p := buildPrompt(nil, tools, "required"); !strings.Contains(p, "You MUST call one of the tools above") {
		t.Fatal("required contract changed")
	}
}
