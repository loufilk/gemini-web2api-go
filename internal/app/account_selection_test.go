package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestAccountSelectionPrefersConfirmedCapability(t *testing.T) {
	installCatalogFixture(t, func(a *CookieAccount) AccountModelCatalog {
		name, hex, mode := "gemini-4-carbon", hexArgon4, 8
		if a.Label == "supported" {
			name, hex, mode = "gemini-4-hydrogen", hexHydrogen4, 1
		}
		return AccountModelCatalog{Models: []DiscoveredModel{{CanonicalName: name, HexID: hex, Mode: mode}}}
	})
	unknown := addTestAccount(t, "unknown")
	for i := 0; i < 4; i++ {
		id := addTestAccount(t, "incompatible")
		if _, err := discoverAccountModels(context.Background(), accountByID(id), ""); err != nil {
			t.Fatal(err)
		}
	}
	good := addTestAccount(t, "supported")
	if _, err := discoverAccountModels(context.Background(), accountByID(good), ""); err != nil {
		t.Fatal(err)
	}
	a, _, err := pickAccountForModel(Models["gemini-4-hydrogen"], nil)
	if err != nil || a == nil || a.ID != good {
		t.Fatalf("preferred supported #%d over unknown #%d: a=%+v err=%v", good, unknown, a, err)
	}
	if accountByID(unknown).LastUsedAt != 0 {
		t.Fatal("ranking touched nonselected account")
	}
	// A stale negative must be re-probed, not treated as a current denial.
	liveModelCatalogs.mu.Lock()
	for _, e := range liveModelCatalogs.entries {
		e.catalog.FetchedAt = time.Now().Add(-modelCatalogTTL - time.Second)
	}
	liveModelCatalogs.mu.Unlock()
	a, _, err = pickAccountForModel(Models["gemini-4-hydrogen"], map[int64]bool{unknown: true, good: true})
	if err != nil || a == nil {
		t.Fatalf("stale entries excluded: %v", err)
	}
}

func TestAccountSelectionUnsupportedIsNotQuotaExhaustion(t *testing.T) {
	installCatalogFixture(t, func(*CookieAccount) AccountModelCatalog {
		return AccountModelCatalog{Models: []DiscoveredModel{{CanonicalName: "gemini-4-carbon", HexID: hexArgon4, Mode: 8}}}
	})
	for i := 0; i < 4; i++ {
		id := addTestAccount(t, "carbon-only")
		if _, err := discoverAccountModels(context.Background(), accountByID(id), ""); err != nil {
			t.Fatal(err)
		}
	}
	a, _, err := pickAccountForModel(Models["gemini-4-argon"], nil)
	var availability *ModelAvailabilityError
	if a != nil || !errors.As(err, &availability) || availability.Status != http.StatusNotFound {
		t.Fatalf("unsupported catalog should be availability not cooldown: %+v %v", a, err)
	}
}

func TestAccountSelectionDoesNotUseOtherCredentialCatalog(t *testing.T) {
	installCatalogFixture(t, func(*CookieAccount) AccountModelCatalog {
		return AccountModelCatalog{Models: []DiscoveredModel{{CanonicalName: "gemini-4-carbon", HexID: hexArgon4, Mode: 8}}}
	})
	id := addTestAccount(t, "rotated")
	if _, err := discoverAccountModels(context.Background(), accountByID(id), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := getDB().Exec(`UPDATE accounts SET cookie=? WHERE id=?`, "SID=new-identity", id); err != nil {
		t.Fatal(err)
	}
	a, _, err := pickAccountForModel(Models["gemini-4-argon"], nil)
	if err != nil || a == nil || a.ID != id {
		t.Fatalf("new credential must be unknown, not stale negative: %+v %v", a, err)
	}
}

// No upstream HTTP is used: XSRF and read-only catalogue RPCs are fixtures.
// Cancel at the fourth candidate to prove the old three-account cutoff is gone
// without submitting a generation request or risking a repeat.
func TestAccountPreparationReachesFourthCandidateWithoutGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	installCatalogFixture(t, func(a *CookieAccount) AccountModelCatalog {
		calls++
		if calls == 4 {
			cancel()
		}
		return AccountModelCatalog{Models: []DiscoveredModel{{CanonicalName: "gemini-4-carbon", HexID: hexArgon4, Mode: 8}}}
	})
	for i := 0; i < 4; i++ {
		id := addTestAccount(t, "candidate")
		a := accountByID(id)
		xsrfMu.Lock()
		xsrfCache[xsrfKey(a.Cookie, a.AuthUser)] = xsrfEntry{token: "fixture-token", fetched: time.Now()}
		xsrfMu.Unlock()
		t.Cleanup(func() { invalidateXSRF(a.Cookie) })
	}
	mc := Models["gemini-4-hydrogen"]
	mc.RequestContext = ctx
	res, err := streamGenerateWithFiles("test", "test", mc, nil, nil, nil)
	if calls != 4 || !errors.Is(err, context.Canceled) || res == nil || res.GenerationAttempts != 0 {
		t.Fatalf("calls=%d res=%+v err=%v", calls, res, err)
	}
}

func TestAccountPreparationBudgetIsNotFalseUnavailable(t *testing.T) {
	calls := 0
	installCatalogFixture(t, func(*CookieAccount) AccountModelCatalog {
		calls++
		return AccountModelCatalog{Models: []DiscoveredModel{{CanonicalName: "gemini-4-carbon", HexID: hexArgon4, Mode: 8}}}
	})
	for i := 0; i < maxAccountPreparations+1; i++ {
		id := addTestAccount(t, "candidate")
		a := accountByID(id)
		xsrfMu.Lock()
		xsrfCache[xsrfKey(a.Cookie, a.AuthUser)] = xsrfEntry{token: "fixture-token", fetched: time.Now()}
		xsrfMu.Unlock()
		t.Cleanup(func() { invalidateXSRF(a.Cookie) })
	}
	res, err := streamGenerateWithFiles("test", "test", Models["gemini-4-hydrogen"], nil, nil, nil)
	var availability *ModelAvailabilityError
	if calls != maxAccountPreparations || !errors.As(err, &availability) || availability.Status != 503 || availability.Code != "model_discovery_budget_exhausted" || res == nil || res.GenerationAttempts != 0 {
		t.Fatalf("calls=%d res=%+v err=%v", calls, res, err)
	}
}

func TestAccountPreparationSwitchesEgressWithAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installCatalogFixture(t, func(*CookieAccount) AccountModelCatalog { return AccountModelCatalog{} })
	proxyMu.Lock()
	old := proxyCache
	proxyCache = []Proxy{{ID: 91001, Name: "a", URL: "http://127.0.0.1:19101", Enabled: true, Weight: 1}, {ID: 91002, Name: "b", URL: "http://127.0.0.1:19102", Enabled: true, Weight: 1}}
	proxyMu.Unlock()
	t.Cleanup(func() { proxyMu.Lock(); proxyCache = old; proxyMu.Unlock() })
	ids := []int64{addTestAccount(t, "egress-a"), addTestAccount(t, "egress-b")}
	expected := map[int64]string{}
	for i, id := range ids {
		pid := int64(91001 + i)
		if _, err := getDB().Exec(`UPDATE accounts SET proxy_id=? WHERE id=?`, pid, id); err != nil {
			t.Fatal(err)
		}
		a := accountByID(id)
		expected[id] = proxyURLByID(pid)
		xsrfMu.Lock()
		xsrfCache[xsrfKey(a.Cookie, a.AuthUser)] = xsrfEntry{token: "fixture", fetched: time.Now()}
		xsrfMu.Unlock()
		t.Cleanup(func() { invalidateXSRF(a.Cookie) })
	}
	calls := 0
	liveModelCatalogs.fetch = func(_ context.Context, a *CookieAccount, proxy string) (AccountModelCatalog, error) {
		calls++
		if proxy != expected[a.ID] {
			t.Errorf("account %d used wrong egress %q", a.ID, proxy)
		}
		if calls == 2 {
			cancel()
		}
		return AccountModelCatalog{Models: []DiscoveredModel{{CanonicalName: "gemini-4-carbon", HexID: hexArgon4, Mode: 8}}}, nil
	}
	mc := Models["gemini-4-hydrogen"]
	mc.RequestContext = ctx
	res, err := streamGenerateWithFiles("test", "test", mc, nil, nil, nil)
	if calls != 2 || !errors.Is(err, context.Canceled) || res == nil || res.AccountID != ids[1] || res.ProxyID != 91002 || res.GenerationAttempts != 0 {
		t.Fatalf("calls=%d res=%+v err=%v", calls, res, err)
	}
}

func TestAccountPreparationDeadlineClassification(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	preparation, stop := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer stop()
	var availability *ModelAvailabilityError
	if err := accountPreparationError(parent, preparation); !errors.As(err, &availability) || availability.Status != 503 || availability.Code != "model_discovery_budget_exhausted" {
		t.Fatalf("internal deadline: %v", err)
	}
	cancel()
	if err := accountPreparationError(parent, preparation); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancel priority: %v", err)
	}
	deadline, stopDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stopDeadline()
	if err := accountPreparationError(deadline, deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent deadline misclassified: %v", err)
	}
}
