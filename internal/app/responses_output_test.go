package app

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesOutputIndicesMatchFinalArray(t *testing.T) {
	withPoolCookie(t)
	setKeepaliveSec(t, 0)
	old := runOpenAIModel
	t.Cleanup(func() { runOpenAIModel = old })
	calls := []ToolCall{{ID: "call_one", Type: "function", Function: ToolCallFunction{Name: "lookup", Arguments: `{"q":"one"}`}}, {ID: "call_two", Type: "function", Function: ToolCallFunction{Name: "lookup", Arguments: `{"q":"two"}`}}}
	for _, tc := range []struct {
		name, text, delta string
		calls             []ToolCall
	}{
		{"mixed_streamed", "Checking now", "Checking now", calls},
		{"mixed_final_only", "Checking now", "", calls},
		{"tools_only", "", "", calls},
		{"tools_after_whitespace", "", " \n", calls},
		{"text_only", "Hello", "Hello", nil},
		{"empty", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runOpenAIModel = func(_ []map[string]interface{}, _, _ string, _ ModelConfig, _ []map[string]interface{}, _ interface{}, _ []pendingUpload, onDelta, _ func(string)) (string, []ToolCall, *StreamResult, error) {
				res := &StreamResult{}
				if tc.delta != "" {
					onDelta(tc.delta)
					res.Emitted = tc.delta
				}
				return tc.text, tc.calls, res, nil
			}
			w := httptest.NewRecorder()
			handleResponses(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gemini-4-hydrogen","stream":true,"input":"hello"}`)))
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			added := map[int]string{}
			done := map[int]string{}
			var output []interface{}
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]interface{}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				typ, _ := event["type"].(string)
				switch typ {
				case "response.output_item.added", "response.output_item.done":
					idx := int(event["output_index"].(float64))
					item := event["item"].(map[string]interface{})
					id := item["id"].(string)
					if typ == "response.output_item.added" {
						if _, ok := added[idx]; ok {
							t.Fatalf("duplicate index %d", idx)
						}
						added[idx] = id
					} else {
						done[idx] = id
					}
				case "response.function_call_arguments.done":
					idx, ok := event["output_index"].(float64)
					if !ok || added[int(idx)] != event["item_id"] {
						t.Fatalf("invalid argument completion: %v", event)
					}
				case "response.completed":
					response := event["response"].(map[string]interface{})
					if response["store"] != false {
						t.Fatalf("no state is stored, response = %v", response)
					}
					output = response["output"].([]interface{})
				}
			}
			if len(output) != len(added) || len(output) != len(done) {
				t.Fatalf("inconsistent item counts: %s", w.Body)
			}
			for i, item := range output {
				id := item.(map[string]interface{})["id"].(string)
				if added[i] != id || done[i] != id {
					t.Fatal(fmt.Sprintf("index %d final %s added %s done %s", i, id, added[i], done[i]))
				}
			}
		})
	}
}
