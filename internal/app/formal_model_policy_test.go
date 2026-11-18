package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func installCatalogFixture(t *testing.T, f func(*CookieAccount) AccountModelCatalog) {
	t.Helper()
	oldFetch := liveModelCatalogs.fetch
	oldEntries := liveModelCatalogs.entries
	liveModelCatalogs.fetch = func(_ context.Context, a *CookieAccount, _ string) (AccountModelCatalog, error) { return f(a), nil }
	liveModelCatalogs.entries = make(map[string]*catalogCacheEntry)
	t.Cleanup(func() { liveModelCatalogs.fetch = oldFetch; liveModelCatalogs.entries = oldEntries })
}

func TestFormalPerAccountRouteRejectsReusedArgonSlot(t *testing.T) {
	installCatalogFixture(t, func(a *CookieAccount) AccountModelCatalog {
		entries := []DiscoveredModel{{CanonicalName: "gemini-4-hydrogen", HexID: hexHydrogen4, Mode: 1}}
		switch a.ID {
		case 111:
			entries = append(entries, DiscoveredModel{CanonicalName: "gemini-4-carbon", HexID: hexArgon4, Mode: 8})
		case 112:
			entries = append(entries, DiscoveredModel{CanonicalName: "gemini-4-argon", HexID: hexArgon4, Mode: 8, ThinkingPolicyKnown: true, ThinkingModes: []int{1, 4}})
		}
		return AccountModelCatalog{Models: entries}
	})
	one, two := &CookieAccount{ID: 111, Cookie: "SID=sim_a", Status: "enabled"}, &CookieAccount{ID: 112, Cookie: "SID=sim_b", Status: "enabled"}
	ctx := context.Background()
	_, err := accountModelRoute(ctx, one, "", Models["gemini-4-argon"])
	var avail *ModelAvailabilityError
	if !errors.As(err, &avail) || avail.Status != http.StatusNotFound {
		t.Fatalf("expected unavailable, got %v", err)
	}
	if r, err := accountModelRoute(ctx, one, "", Models["gemini-4-carbon"]); err != nil || r.Upstream != "4 Carbon" || r.Mode != 8 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := accountModelRoute(ctx, two, "", Models["gemini-4-argon-thinking-medium"]); err != nil {
		t.Fatal(err)
	}
	if _, err := accountModelRoute(ctx, two, "", Models["gemini-4-argon-thinking"]); !errors.As(err, &avail) {
		t.Fatalf("unsupported thinking passed: %v", err)
	}
}

func TestFormalStreamIdentityWithholdsEarlyContentAndRejectsLateMismatch(t *testing.T) {
	mc := Models["gemini-4-hydrogen"]
	var lines []string
	emit := guardedModelEmitter(mc, func(line string) error { lines = append(lines, line); return nil })
	if err := emit(identityFrame("", false)); err != nil || len(lines) != 0 {
		t.Fatalf("unconfirmed line escaped: %v %+v", err, lines)
	}
	if err := emit(identityFrame("4 Hydrogen", false)); err != nil || len(lines) != 2 {
		t.Fatalf("confirmed prefix not emitted: %v %d", err, len(lines))
	}
	if err := emit(identityFrame("4 Carbon", false)); err == nil || len(lines) != 2 {
		t.Fatalf("wrong model line escaped: %v %d", err, len(lines))
	}
	pro := Models["gemini-3.1-pro"]
	_, err := verifyModelIdentity(pro, identityFrame("3.5 Flash-Lite", false))
	var down *ModelDowngradedError
	if !errors.As(modelMismatchForAccount(err, 123), &down) || down.AccountID != 123 {
		t.Fatalf("Pro downgrade not attributed: %v", err)
	}
}

func TestFormalOldTransportCancellationDoesNotResubmit(t *testing.T) {
	var submits atomic.Int32
	started := make(chan struct{})
	stopped := make(chan struct{})
	stopHandler := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if submits.Add(1) == 1 {
			close(started)
		}
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-stopHandler:
		}
	}))
	defer s.Close()
	defer close(stopHandler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := doGeminiRequestContext(ctx, "http://upstream.invalid/generate", "test", nil, s.URL, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("mock did not see submission")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("mock connection not canceled")
	}
	if submits.Load() != 1 {
		t.Fatal("unexpected resubmission")
	}
}

func TestFormalLogRequestedModelDropsUnexpectedSuffix(t *testing.T) {
	got := safeRequestedModel("gemini-3.8-flash@think=secret-token", "gemini-4-hydrogen")
	if got != "gemini-3.8-flash" || strings.Contains(got, "secret") {
		t.Fatal(got)
	}
}

// Upstream retirement wins over stale/per-account discovery data. In particular,
// the dynamic catalog must not re-add a model omitted by availableModels.
func TestRetiredModelNotReintroducedByDiscoveredCatalog(t *testing.T) {
	withPoolCookie(t)
	installCatalogFixture(t, func(*CookieAccount) AccountModelCatalog {
		return AccountModelCatalog{Models: []DiscoveredModel{
			{CanonicalName: "gemini-3.1-pro", HexID: hexPro31, Mode: 3, ThinkingPolicyKnown: true, ThinkingModes: []int{1, 2}},
			{CanonicalName: "gemini-4-hydrogen", HexID: hexHydrogen4, Mode: 1},
		}}
	})
	got := catalogModels(context.Background())
	for _, name := range []string{"gemini-3.1-pro", "gemini-3.1-pro-thinking"} {
		if _, ok := got[name]; ok {
			t.Fatalf("retired model was advertised: %s", name)
		}
	}
	if _, ok := got["gemini-4-hydrogen"]; !ok {
		t.Fatal("active discovered model was omitted")
	}
}
