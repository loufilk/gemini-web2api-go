package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func responseInput(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestResponsesNormalizeToolRoundTrip(t *testing.T) {
	req := responseInput(t, `{"instructions":"keep this","input":[
	{"role":"developer","content":"developer instruction"},
	{"role":"user","content":"read file"},
	{"type":"function_call","call_id":"call_one","name":"read_file","arguments":"{\"path\":\"first.txt\"}"},
	{"type":"function_call","call_id":"call_two","name":"read_file","arguments":"{\"path\":\"second.txt\"}"},
	{"type":"function_call_output","call_id":"call_two","output":"second output"},
	{"type":"function_call_output","call_id":"call_one","output":[{"type":"input_text","text":"first output"}]},
	{"type":"message","role":"assistant","content":[{"type":"output_text","text":"read both"}]}
	]}`)
	msgs, err := normalizeResponsesInput(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 8 || msgs[1]["role"] != "system" {
		t.Fatalf("messages: %#v", msgs)
	}
	for _, idx := range []int{3, 4} {
		if calls, ok := msgs[idx]["tool_calls"].([]interface{}); !ok || len(calls) != 1 {
			t.Fatalf("tool history lost: %#v", msgs[idx])
		}
	}
	if msgs[5]["tool_call_id"] != "call_two" || msgs[6]["tool_call_id"] != "call_one" || msgs[5]["name"] != "read_file" {
		t.Fatalf("tool attribution lost: %#v", msgs)
	}
	prompt, _ := messagesToPrompt(msgs, nil, nil)
	for _, want := range []string{"keep this", "developer instruction", "first.txt", "second.txt", "first output", "second output", "read both", `"call_id": "call_one"`, "call_id=call_two"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lost %q: %s", want, prompt)
		}
	}
}

func TestResponsesNestedCallsAndMultimodalPreserved(t *testing.T) {
	req := responseInput(t, `{"input":[
	{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},
	{"role":"assistant","content":[{"type":"output_text","text":"looking"},{"type":"function_call","call_id":"nested","name":"inspect","arguments":"{}"}]},
	{"type":"function_call_output","call_id":"nested","output":[{"type":"input_text","text":"result"},{"type":"input_image","image_url":"data:image/png;base64,d29ybGQ="}]}
	]}`)
	msgs, err := normalizeResponsesInput(req)
	if err != nil {
		t.Fatal(err)
	}
	images, err := collectImages(msgs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || string(images[0].Data) != "hello" || string(images[1].Data) != "world" {
		t.Fatalf("images lost: %#v", images)
	}
	prompt, _ := messagesToPrompt(msgs, nil, nil)
	if !strings.Contains(prompt, "inspect") || !strings.Contains(prompt, "looking") || !strings.Contains(prompt, "result") {
		t.Fatal(prompt)
	}
}

func TestResponsesRejectUnsupportedContinuationAndLossyInputs(t *testing.T) {
	for _, body := range []string{
		`{"previous_response_id":"resp_old","input":"next"}`,
		`{"previous_response_id":12,"input":"next"}`,
		`{"store":true,"input":"next"}`,
		`{"store":"false","input":"next"}`,
		`{"conversation":"conv_old","input":"next"}`,
		`{"conversation":{"id":"conv_old"},"input":"next"}`,
		`{"input":[{"type":"item_reference","id":"old"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"old"}]}]}`,
		`{"input":[{"type":"function_call_output","call_id":"id","output":42}]}`,
		`{"input":[{"type":"function_call","name":"bad","arguments":"{}"}]}`,
		`{"instructions":17,"input":"next"}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file_old"}]}]}`,
	} {
		if _, err := normalizeResponsesInput(responseInput(t, body)); err == nil {
			t.Errorf("accepted unsupported input: %s", body)
		}
	}
	for _, body := range []string{`{"previous_response_id":null,"input":"ok"}`, `{"input":"ok"}`, `{"store":false,"conversation":null,"input":"ok"}`} {
		if _, err := normalizeResponsesInput(responseInput(t, body)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResponsesContinuationRejectedBeforeGeneration(t *testing.T) {
	old := runOpenAIModel
	t.Cleanup(func() { runOpenAIModel = old })
	runOpenAIModel = func(_ []map[string]interface{}, _, _ string, _ ModelConfig, _ []map[string]interface{}, _ interface{}, _ []pendingUpload, _, _ func(string)) (string, []ToolCall, *StreamResult, error) {
		t.Fatal("unsupported continuation reached generation")
		return "", nil, nil, nil
	}
	for _, stream := range []string{"true", "false"} {
		w := httptest.NewRecorder()
		handleResponses(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gemini-4-hydrogen","input":"next","previous_response_id":"resp_old","stream":`+stream+`}`)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "previous_response_id") || strings.Contains(w.Body.String(), "response.created") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	}
}

func TestResponsesToolChoice(t *testing.T) {
	for _, selector := range []string{
		`{"type":"function","name":"lookup"}`,
		`{"type":"function","function":{"name":"lookup"}}`,
	} {
		req := responseInput(t, `{"input":"run","tools":[{"type":"function","name":"lookup","parameters":{}}],"tool_choice":`+selector+`}`)
		if _, err := normalizeResponsesInput(req); err != nil {
			t.Fatal(err)
		}
		mode, name := parseToolChoice(req["tool_choice"])
		if mode != "required" || name != "lookup" {
			t.Fatalf("%s %s", mode, name)
		}
	}
	for _, selector := range []string{`{"type":"web_search"}`, `{"type":"function","name":"missing"}`, `"unsupported"`} {
		req := responseInput(t, `{"input":"run","tool_choice":`+selector+`}`)
		if _, err := normalizeResponsesInput(req); err == nil {
			t.Errorf("accepted %s", selector)
		}
	}
}

func TestResponsesHTTPPreservesHistoryAndImages(t *testing.T) {
	withPoolCookie(t)
	setKeepaliveSec(t, 0)
	old := runOpenAIModel
	t.Cleanup(func() { runOpenAIModel = old })
	calls := 0
	runOpenAIModel = func(messages []map[string]interface{}, prompt, _ string, mc ModelConfig, _ []map[string]interface{}, _ interface{}, images []pendingUpload, onDelta, _ func(string)) (string, []ToolCall, *StreamResult, error) {
		calls++
		if len(images) != 1 || string(images[0].Data) != "hello" || len(messages) != 3 {
			t.Fatalf("lost input: images=%#v messages=%#v", images, messages)
		}
		if !strings.Contains(prompt, "first.txt") || !strings.Contains(prompt, "call_id=call_one") || !strings.Contains(prompt, "file contents") {
			t.Fatal(prompt)
		}
		if onDelta != nil {
			onDelta("done")
		}
		return "done", nil, &StreamResult{Emitted: "done"}, nil
	}
	for _, stream := range []string{"true", "false"} {
		body := `{"model":"gemini-3.6-flash","stream":` + stream + `,"input":[
			{"role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},
			{"type":"function_call","call_id":"call_one","name":"read_file","arguments":"{\"path\":\"first.txt\"}"},
			{"type":"function_call_output","call_id":"call_one","output":"file contents"}]}`
		w := httptest.NewRecorder()
		handleResponses(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "done") {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("generation count %d", calls)
	}
}

func TestNativeHistoryPreservesOnlyRealCallIDs(t *testing.T) {
	for _, id := range []string{"", "native_call"} {
		idField := ""
		if id != "" {
			idField = `"id":"` + id + `",`
		}
		var req geminiAPIRequest
		body := `{"contents":[{"role":"model","parts":[{"functionCall":{` + idField + `"name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{` + idField + `"name":"lookup","response":{"result":"done"}}}]}]}`
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		msgs, tools, choice, err := geminiAPIToMessages(&req)
		if err != nil {
			t.Fatal(err)
		}
		prompt, _ := messagesToPrompt(msgs, tools, choice)
		if id == "" {
			if strings.Contains(prompt, "call_id") {
				t.Fatalf("fabricated identity: %s", prompt)
			}
		} else if !strings.Contains(prompt, `"call_id": "native_call"`) || !strings.Contains(prompt, "call_id=native_call") {
			t.Fatalf("lost identity: %s", prompt)
		}
	}
}
