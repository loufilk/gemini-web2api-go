package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFormalSSEFailureIsTerminal(t *testing.T) {
	w := httptest.NewRecorder()
	s := newSSEWriter(w, "id", 1, "model")
	s.Ping()
	s.Fail(errors.New("upstream_timeout"))
	before := w.Body.String()
	s.SendContent("must not appear")
	s.Finish("stop", nil)
	s.Ping()
	s.Fail(errors.New("again"))
	if w.Body.String() != before || strings.Contains(before, "[DONE]") || !strings.Contains(before, "upstream_timeout") {
		t.Fatalf("invalid terminal: %s", w.Body.String())
	}
}

func TestFormalHTTPContextAndCancellation(t *testing.T) {
	withPoolCookie(t)
	setKeepaliveSec(t, 0)
	old, oldNative := runOpenAIModel, runGeminiCall
	t.Cleanup(func() { runOpenAIModel, runGeminiCall = old, oldNative })
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	check := func(mc ModelConfig) (string, []ToolCall, *StreamResult, error) {
		if mc.RequestContext == nil || mc.RequestContext.Value(ctxKey{}) != "sentinel" {
			t.Fatal("request context lost")
		}
		return "", nil, nil, context.Canceled
	}
	runOpenAIModel = func(_ []map[string]interface{}, _, _ string, mc ModelConfig, _ []map[string]interface{}, _ interface{}, _ []pendingUpload, _, _ func(string)) (string, []ToolCall, *StreamResult, error) {
		return check(mc)
	}
	runGeminiCall = func(g *geminiCall, _, _ func(string)) (string, []ToolCall, *StreamResult, error) {
		return check(g.modelCfg)
	}
	for _, route := range []struct {
		path, body string
		handler    http.HandlerFunc
	}{
		{"/v1/chat/completions", `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hello"}]}`, handleChatCompletions},
		{"/v1/responses", `{"model":"gemini-4-argon","input":"hello"}`, handleResponses},
		{"/v1beta/models/gemini-4-argon:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}]}`, handleGeminiModelItem},
	} {
		w := httptest.NewRecorder()
		route.handler(w, httptest.NewRequest("POST", route.path, strings.NewReader(route.body)).WithContext(ctx))
		if w.Body.Len() != 0 {
			t.Fatalf("cancel wrote a body: %s", w.Body.String())
		}
	}
}

func TestFormalIdentityWaitKeepsAliveThenFails(t *testing.T) {
	withPoolCookie(t)
	setKeepaliveSec(t, 1) // accelerated version of configurable 15-second cadence
	old, oldNative := runOpenAIModel, runGeminiCall
	t.Cleanup(func() { runOpenAIModel, runGeminiCall = old, oldNative })
	fail := func() (string, []ToolCall, *StreamResult, error) {
		time.Sleep(1200 * time.Millisecond)
		return "", nil, nil, errors.New("private upstream body secret")
	}
	runOpenAIModel = func(_ []map[string]interface{}, _, _ string, _ ModelConfig, _ []map[string]interface{}, _ interface{}, _ []pendingUpload, _, _ func(string)) (string, []ToolCall, *StreamResult, error) {
		return fail()
	}
	runGeminiCall = func(_ *geminiCall, _, _ func(string)) (string, []ToolCall, *StreamResult, error) { return fail() }
	for _, route := range []struct {
		path, body, terminal string
		handler              http.HandlerFunc
	}{
		{"/v1/chat/completions", `{"model":"gemini-4-argon","stream":true,"messages":[{"role":"user","content":"hello"}]}`, `"error"`, handleChatCompletions},
		{"/v1/responses", `{"model":"gemini-4-argon","stream":true,"input":"hello"}`, "response.failed", handleResponses},
		{"/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hello"}]}]}`, `"error"`, handleGeminiModelItem},
	} {
		w := httptest.NewRecorder()
		route.handler(w, httptest.NewRequest("POST", route.path, strings.NewReader(route.body)))
		b := w.Body.String()
		if w.Code != 200 || !w.Flushed || !strings.Contains(b, route.terminal) || strings.Contains(b, "private upstream") || strings.Contains(b, "[DONE]") || strings.Contains(b, "response.completed") || strings.Contains(b, `"finishReason":"STOP"`) {
			t.Fatalf("%s: %d %s", route.path, w.Code, b)
		}
	}
}

func TestFormalGenerationFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{context.Canceled, 499, "client_canceled"}, {context.DeadlineExceeded, 504, "client_deadline"}, {&generationPhaseTimeout{Phase: "first_frame"}, 504, "upstream_first_frame_timeout"}, {&generationPhaseTimeout{Phase: "idle"}, 504, "upstream_idle_timeout"}, {&generationPhaseTimeout{Phase: "overall"}, 504, "upstream_overall_timeout"}, {errors.New("secret"), 502, "upstream_error"},
	} {
		status, code := generationFailure(tc.err)
		if status != tc.status || code != tc.code {
			t.Fatalf("%d %s", status, code)
		}
	}
}

func TestFormalGeminiFailureIsTerminal(t *testing.T) {
	for _, sse := range []bool{false, true} {
		w := httptest.NewRecorder()
		s := &geminiStream{w: w, flusher: w, sse: sse, model: "model", rid: "id"}
		s.ping()
		s.fail(errors.New("upstream_error"))
		before := w.Body.String()
		s.text("must not appear")
		s.ping()
		s.end()
		if w.Body.String() != before || strings.Contains(before, `"finishReason":"STOP"`) {
			t.Fatalf("invalid terminal %s", w.Body.String())
		}
	}
}
