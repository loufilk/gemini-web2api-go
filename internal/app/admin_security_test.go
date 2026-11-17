package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminRequiresConfiguredToken(t *testing.T) {
	old := cfg.AdminToken
	t.Cleanup(func() { cfg.AdminToken = old })
	for _, token := range []string{"", "   "} {
		cfg.AdminToken = token
		r := httptest.NewRequest("GET", "http://localhost/admin/api/config?token=x", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		if authOK(r) {
			t.Fatal("unconfigured admin authenticated")
		}
		w := httptest.NewRecorder()
		handleAdminLogin(w, httptest.NewRequest("POST", "/admin/api/login", strings.NewReader(`{"token":""}`)))
		if w.Code != 503 || len(w.Result().Cookies()) != 0 {
			t.Fatalf("unconfigured login: %d", w.Code)
		}
	}
	cfg.AdminToken = "test-admin-token"
	if authOK(httptest.NewRequest("GET", "/admin/api/config?token=test-admin-token", nil)) {
		t.Fatal("URL query credentials must not authenticate")
	}
	for _, supplied := range []string{"wrong", "test-admin-token"} {
		r := httptest.NewRequest("GET", "/admin/api/config", nil)
		r.Header.Set("Authorization", "Bearer "+supplied)
		if authOK(r) != (supplied == cfg.AdminToken) {
			t.Fatal("bearer authentication mismatch")
		}
	}
}

func TestAdminLoginBoundsAndMethod(t *testing.T) {
	old := cfg.AdminToken
	cfg.AdminToken = "test-admin-token"
	t.Cleanup(func() { cfg.AdminToken = old })
	for _, tc := range []struct {
		method, body string
		status       int
	}{
		{"GET", `{"token":"test-admin-token"}`, http.StatusMethodNotAllowed},
		{"POST", strings.Repeat(" ", 4097), http.StatusBadRequest},
		{"POST", `{"token":"wrong"}`, http.StatusUnauthorized},
	} {
		w := httptest.NewRecorder()
		handleAdminLogin(w, httptest.NewRequest(tc.method, "/admin/api/login", strings.NewReader(tc.body)))
		if w.Code != tc.status || len(w.Result().Cookies()) != 0 {
			t.Fatalf("login code=%d expected %d", w.Code, tc.status)
		}
	}
}

func TestStatsWindowBoundedAndPercentilesHonest(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 24}, {"-1", 1}, {"0", 1}, {"999999", 720}, {"24", 24}} {
		if got := boundedStatsHours(tc.raw); got != tc.want {
			t.Fatalf("%s: %d", tc.raw, got)
		}
	}
	if percentileDisplayValue("24h", 0) != nil || percentileDisplayValue("bad", 0) != nil {
		t.Fatal("missing percentile must be null")
	}
	if percentileMethod("7d") != "mean_of_bucket_percentiles" || percentileMethod("30d") != "mean_of_bucket_percentiles" {
		t.Fatal("aggregate method not labeled")
	}
}
