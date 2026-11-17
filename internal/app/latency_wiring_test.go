package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLatencyOnlyGenerationTransportMarksSubmission(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, okRaw)
	}))
	defer s.Close()
	ctx := withRequestLatency(context.Background())
	if status, _, _, _, err := doGeminiRequestContext(ctx, "http://fixture.invalid/catalog", "test", nil, s.URL, nil); err != nil || status != 200 {
		t.Fatalf("catalog: %d %v", status, err)
	}
	first := snapshotRequestLatency(ctx)
	if first.Submitted != nil || first.UpstreamText != nil {
		t.Fatalf("catalog marked generation: %+v", first)
	}
	if status, _, _, _, err := doGeminiRequestContext(modelGenerationContext(ctx), "http://fixture.invalid/generation", "test", nil, s.URL, nil); err != nil || status != 200 {
		t.Fatalf("generation: %d %v", status, err)
	}
	got := snapshotRequestLatency(ctx)
	if got.Submitted == nil || got.UpstreamText == nil || *got.Submitted > *got.UpstreamText {
		t.Fatalf("timing order: %+v", got)
	}
}

func TestLatencyChatPersistsTextAndToolSeparately(t *testing.T) {
	setKeepaliveSec(t, 0)
	old := runOpenAIModel
	t.Cleanup(func() { runOpenAIModel = old })
	var before int64
	_ = getDB().QueryRow("SELECT COALESCE(MAX(id),0) FROM requests").Scan(&before)
	runOpenAIModel = func(_ []map[string]interface{}, prompt, _ string, mc ModelConfig, _ []map[string]interface{}, _ interface{}, _ []pendingUpload, onDelta, _ func(string)) (string, []ToolCall, *StreamResult, error) {
		ctx := modelRequestContext(mc)
		noteRequestPrompt(ctx, len(prompt), 128000)
		noteContextAttachment(ctx, false)
		markRequestLatency(ctx, latencySubmitted)
		markRequestLatency(ctx, latencyUpstreamText)
		onDelta("visible")
		timed := snapshotRequestLatency(ctx)
		if timed.DownstreamText == nil || timed.Tool != nil {
			t.Fatalf("text was not marked independently of tool: %+v", timed)
		}
		return "visible", []ToolCall{{ID: "call_fixture", Type: "function", Function: ToolCallFunction{Name: "fixture", Arguments: "{}"}}}, &StreamResult{Emitted: "visible"}, nil
	}
	body := `{"model":"gemini-3.6-flash","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	handleChatCompletions(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if !strings.Contains(w.Body.String(), "call_fixture") {
		t.Fatal(w.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ts, tool, submitted, source, beat, bytes sql.NullInt64
		var attachment sql.NullBool
		err := getDB().QueryRow("SELECT downstream_first_text_ms, first_tool_ms, generation_start_ms, upstream_first_text_ms, first_heartbeat_ms, prompt_bytes, context_attachment_used FROM requests WHERE id>? AND endpoint='chat.completions' ORDER BY id DESC LIMIT 1", before).Scan(&ts, &tool, &submitted, &source, &beat, &bytes, &attachment)
		if err == sql.ErrNoRows {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !ts.Valid || !tool.Valid || !submitted.Valid || !source.Valid || !bytes.Valid || !attachment.Valid || attachment.Bool || beat.Valid || ts.Int64 > tool.Int64 || submitted.Int64 > source.Int64 {
			b, _ := json.Marshal([]any{ts, tool, submitted, source, beat, bytes, attachment})
			t.Fatalf("unexpected stored timings %s", b)
		}
		return
	}
	t.Fatal("timed request row was not persisted")
}
