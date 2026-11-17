package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func identityFrame(reported string, nested bool) string {
	inner := make([]interface{}, 43)
	inner[4] = []interface{}{[]interface{}{"candidate", []interface{}{"answer"}}}
	if nested {
		inner[4] = []interface{}{[]interface{}{"fbb127bbb056c959", nil, nil, reported}}
	} else if reported != "" {
		inner[39], inner[42] = "fbb127bbb056c959", reported
	}
	data, _ := json.Marshal(inner)
	frame, _ := json.Marshal([][]interface{}{{"wrb.fr", nil, string(data)}})
	return string(frame)
}

func TestModelIdentityRejectsWrongUnknownAndLateContradiction(t *testing.T) {
	setStrictModel(t, false)
	for _, name := range []string{"gemini-4-argon", "gemini-4-argon-thinking", "gemini-4-argon-thinking-medium"} {
		_, err := verifyModelIdentity(Models[name], identityFrame("4 Carbon", false))
		var got *ModelIdentityError
		if !errors.As(err, &got) || got.code() != "model_identity_mismatch" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	h := Models["gemini-4-hydrogen"]
	for _, tc := range []struct{ label, raw, code string }{
		{"matching", identityFrame("4 Hydrogen", false), ""},
		{"known suffix", identityFrame("4 Hydrogen Thinking High", false), ""},
		{"absent", identityFrame("", false), "model_identity_unverified"},
		{"nested non-metadata", identityFrame("4 Hydrogen", true), "model_identity_unverified"},
		{"stale", identityFrame("3.8 Flash", false), "model_identity_mismatch"},
		{"lookalike", identityFrame("4 Hydrogen Carbon", false), "model_identity_mismatch"},
		{"late contradiction", identityFrame("4 Hydrogen", false) + "\n122\n" + identityFrame("4 Carbon", false), "model_identity_mismatch"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, err := verifyModelIdentity(h, tc.raw)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var got *ModelIdentityError
			if !errors.As(err, &got) || got.code() != tc.code {
				t.Fatalf("%v, want %s", err, tc.code)
			}
			w := httptest.NewRecorder()
			writeModelIdentityError(w, got)
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if _, err := verifyModelIdentity(Models["gemini-4-carbon"], identityFrame("4 Carbon", false)); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityParsesLegacyAndWebChannelMetadata(t *testing.T) {
	a, b := responseModelReports(okRaw), responseModelReports(wcClassify([]byte(wcContentPayload("0"))).Line)
	if len(a) != 1 || a[0] != "3.6 Flash" || len(b) != 1 || b[0] != "3.6 Flash" {
		t.Fatalf("legacy=%v webchannel=%v", a, b)
	}
}

func TestHydrogenAliasesAndArgonCatalog(t *testing.T) {
	withPoolCookie(t)
	for old, want := range map[string]string{
		"gemini-3.8-flash": "gemini-4-hydrogen", "gemini-3.7-flash": "gemini-4-hydrogen",
		"gemini-3.8-flash-thinking": "gemini-4-hydrogen-thinking", "gemini-3.7-flash-thinking": "gemini-4-hydrogen-thinking",
	} {
		name, mc, err := resolveModel(old)
		if err != nil || name != want || !mc.RequireModelIdentity || mc.Upstream != "4 Hydrogen" {
			t.Fatalf("%s: %s %+v %v", old, name, mc, err)
		}
		if _, ok := availableModels()[old]; ok {
			t.Fatalf("legacy name %s is still advertised", old)
		}
	}
	if _, ok := availableModels()["gemini-4-argon"]; ok {
		t.Fatal("Argon must not be advertised without account capability proof")
	}
	if _, mc, err := resolveModel("gemini-4-argon"); err != nil || !mc.RequireModelIdentity {
		t.Fatalf("existing Argon callers must get a guarded route: %+v %v", mc, err)
	}
	if _, ok := availableModels()["gemini-4-carbon"]; !ok {
		t.Fatal("missing explicit Carbon catalog entry")
	}
}

func TestVerifiedStreamDoesNotCommitWhenUpstreamIdentityMismatches(t *testing.T) {
	withPoolCookie(t)
	for _, route := range []struct {
		path string
		body string
		fn   http.HandlerFunc
	}{
		{"/v1/chat/completions", `{"model":"gemini-4-argon","stream":true,"messages":[{"role":"user","content":"hello"}]}`, handleChatCompletions},
		{"/v1/responses", `{"model":"gemini-4-argon","stream":true,"input":"hello"}`, handleResponses},
		{"/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", `{"contents":[{"parts":[{"text":"hello"}]}]}`, handleGeminiModelItem},
	} {
		t.Run(route.path, func(t *testing.T) {
			old, oldNative := runOpenAIModel, runGeminiCall
			t.Cleanup(func() { runOpenAIModel, runGeminiCall = old, oldNative })
			w := httptest.NewRecorder()
			verify := func(mc ModelConfig) (string, []ToolCall, *StreamResult, error) {
				if w.Body.Len() != 0 || w.Flushed || w.Header().Get("Content-Type") != "" {
					t.Fatal("client response was committed before verification")
				}
				reported, err := verifyModelIdentity(mc, identityFrame("4 Carbon", false))
				return "", nil, &StreamResult{UpstreamModel: reported}, err
			}
			runOpenAIModel = func(_ []map[string]interface{}, _, _ string, mc ModelConfig, _ []map[string]interface{}, _ interface{}, _ []pendingUpload, _, _ func(string)) (string, []ToolCall, *StreamResult, error) {
				return verify(mc)
			}
			runGeminiCall = func(g *geminiCall, _, _ func(string)) (string, []ToolCall, *StreamResult, error) {
				return verify(g.modelCfg)
			}
			route.fn(w, httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body)))
			if w.Code != http.StatusBadGateway || w.Flushed || !strings.Contains(w.Body.String(), "4 Carbon") || strings.Contains(w.Body.String(), "response.created") {
				t.Fatalf("status=%d flushed=%t body=%s", w.Code, w.Flushed, w.Body)
			}
		})
	}
}
