package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discoveryFixture(policy bool) []any {
	p := make([]any, 25)
	row := func(hex, category, product string, mode int) []any {
		r := make([]any, 20)
		r[0] = hex
		r[1] = category
		r[11] = "Most complex work"
		r[12] = product
		r[17] = mode
		return r
	}
	p[15] = []any{row("95a4c0abcba24364", "Carbon", "4 Carbon", 8), row("56fdd199312815e2", "4 Hydrogen", "4 Hydrogen", 1)}
	p[16] = []any{16}
	p[17] = []any{106}
	if policy {
		p[24] = []any{"policy", []any{[]any{[]any{"", 8}, []any{[]any{1, "Low"}, []any{4, "Medium"}, []any{2, "High"}, []any{99, "Unknown"}}}, []any{[]any{"", 1}, []any{[]any{1, "Low"}, []any{2, "High"}}}}}
	}
	return p
}
func discoveryBody(p []any, rpc string) []byte {
	b, _ := json.Marshal(p)
	out, _ := json.Marshal([]any{[]any{"wrb.fr", rpc, string(b), nil}})
	return append([]byte(")]}'\n123\n"), out...)
}
func TestAccountModelCatalogPinnedSchema(t *testing.T) {
	c, err := parseAccountModelCatalog(discoveryBody(discoveryFixture(true), "otAQ7b"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := c.Lookup("gemini-4-carbon")
	if !ok || m.HexID != "95a4c0abcba24364" || m.Mode != 8 || m.Capacity != 3 || m.CapacityField != 12 {
		t.Fatalf("bad model: %+v", m)
	}
	if !m.SupportsThinking(4) || m.SupportsThinking(99) {
		t.Fatal("invalid thinking policy")
	}
	if _, ok := c.Lookup("gemini-4-argon"); ok {
		t.Fatal("reused hex must not invent Argon")
	}
	h, _ := c.Lookup("gemini-4-hydrogen")
	if h.SupportsThinking(4) {
		t.Fatal("thinking leaked between modes")
	}
	// Copy-out prevents callers mutating cached policy.
	m.ThinkingModes[0] = 99
	m2, _ := c.Lookup("gemini-4-carbon")
	if !m2.SupportsThinking(1) {
		t.Fatal("aliased policy")
	}
}
func TestAccountModelCatalogUnknownPolicyClosed(t *testing.T) {
	for _, policy := range []any{nil, []any{"empty", []any{}}} {
		p := discoveryFixture(false)
		p[24] = policy
		c, err := parseAccountModelCatalog(discoveryBody(p, "otAQ7b"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range c.Models {
			if m.SupportsThinking(1) || m.SupportsThinking(2) {
				t.Fatal("guessed capability")
			}
			if m.ThinkingPolicyKnown != (policy != nil) {
				t.Fatal("absent vs empty policy lost")
			}
		}
	}
}
func TestAccountModelCatalogRejectsMalformed(t *testing.T) {
	cases := map[string]func([]any){
		"missing list":    func(p []any) { p[15] = nil },
		"missing mode":    func(p []any) { p[15].([]any)[0].([]any)[17] = nil },
		"fractional mode": func(p []any) { p[15].([]any)[0].([]any)[17] = 1.5 },
		"bad hex":         func(p []any) { p[15].([]any)[0].([]any)[0] = "bad" },
		"duplicate hex":   func(p []any) { p[15] = append(p[15].([]any), p[15].([]any)[0]) },
		"bad policy":      func(p []any) { p[24] = []any{"policy", "bad"} },
		"bad flags":       func(p []any) { p[16] = "bad" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := discoveryFixture(true)
			mutate(p)
			if _, err := parseAccountModelCatalog(discoveryBody(p, "otAQ7b")); err == nil {
				t.Fatal("accepted malformed catalog")
			}
		})
	}
	if _, err := parseAccountModelCatalog(discoveryBody(discoveryFixture(true), "unrelated")); err == nil {
		t.Fatal("accepted another RPC")
	}
	// A policy mentioning a model does NOT make that model routable.
	p := discoveryFixture(true)
	p[15] = []any{p[15].([]any)[1]}
	c, err := parseAccountModelCatalog(discoveryBody(p, "otAQ7b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("gemini-4-carbon"); ok {
		t.Fatal("policy invented catalog model")
	}
}
func TestAccountModelCatalogCacheIsolationExpiry(t *testing.T) {
	now := time.Unix(100000, 0)
	var calls int
	fail := false
	c := accountCatalogCache{entries: map[string]*catalogCacheEntry{}, now: func() time.Time { return now }, fetch: func(context.Context, *CookieAccount, string) (AccountModelCatalog, error) {
		calls++
		if fail {
			return AccountModelCatalog{}, errModelCatalogTransient
		}
		return parseAccountModelCatalog(discoveryBody(discoveryFixture(true), "otAQ7b"))
	}}
	a := &CookieAccount{ID: 1, Cookie: "synthetic-not-a-credential"}
	out, err := c.get(context.Background(), a, "")
	if err != nil || out.Stale || calls != 1 {
		t.Fatal(out, err, calls)
	}
	out.Models[0].DisplayName = "modified"
	out.Models[0].ThinkingModes[0] = 99
	out, err = c.get(context.Background(), a, "")
	if err != nil || calls != 1 || out.Models[0].DisplayName == "modified" || !out.Models[0].SupportsThinking(1) {
		t.Fatal("cache alias or miss")
	}
	for _, b := range []CookieAccount{{ID: 2, Cookie: a.Cookie}, {ID: 1, Cookie: a.Cookie, AuthUser: 1}, {ID: 1, Cookie: "rotated"}} {
		if _, ok := c.cached(&b, ""); ok {
			t.Fatal("cross-account cache leak")
		}
	}
	if _, ok := c.cached(a, "another-proxy"); ok {
		t.Fatal("cross-proxy cache leak")
	}
	now = now.Add(modelCatalogTTL)
	fail = true
	out, err = c.get(context.Background(), a, "")
	if err != nil || !out.Stale || calls != 2 {
		t.Fatal("bounded stale missing", err, calls)
	}
	_, _ = c.get(context.Background(), a, "")
	if calls != 2 {
		t.Fatal("failure cooldown not applied")
	}
	now = now.Add(modelCatalogMaxAge)
	if _, err := c.get(context.Background(), a, ""); err == nil {
		t.Fatal("stale catalog retained forever")
	}
	if _, ok := c.cached(a, ""); ok {
		t.Fatal("expired cached catalog exposed")
	}
}
func TestAccountModelCatalogSingleflightAndCancellation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	c := accountCatalogCache{entries: map[string]*catalogCacheEntry{}, now: time.Now, fetch: func(context.Context, *CookieAccount, string) (AccountModelCatalog, error) {
		calls.Add(1)
		close(started)
		<-release
		return parseAccountModelCatalog(discoveryBody(discoveryFixture(true), "otAQ7b"))
	}}
	a := &CookieAccount{ID: 1, Cookie: "synthetic"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.get(context.Background(), a, ""); err != nil {
			t.Error(err)
		}
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, a, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.get(context.Background(), a, ""); err != nil {
				t.Error(err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate refresh", calls.Load())
	}
}

func TestAccountModelCatalogIdentityRotation(t *testing.T) {
	a := &CookieAccount{ID: 1, Cookie: "SID=identity; SAPISID=auth; SIDCC=old; __Secure-1PSIDTS=old"}
	b := *a
	b.Cookie = "SIDCC=new; SAPISID=auth; SID=identity; __Secure-1PSIDTS=new"
	ak, _ := catalogAccountKey(a, "")
	bk, _ := catalogAccountKey(&b, "")
	if ak != bk {
		t.Fatal("routine refresh invalidates catalog")
	}
	b.Cookie = "SID=other; SAPISID=auth"
	bk, _ = catalogAccountKey(&b, "")
	if ak == bk {
		t.Fatal("account identity change does not invalidate")
	}
}
func TestAccountModelCatalogPermanentFailureInvalidates(t *testing.T) {
	now := time.Unix(100000, 0)
	fail := false
	c := accountCatalogCache{entries: map[string]*catalogCacheEntry{}, now: func() time.Time { return now }, fetch: func(context.Context, *CookieAccount, string) (AccountModelCatalog, error) {
		if fail {
			return AccountModelCatalog{}, errModelCatalog
		}
		return parseAccountModelCatalog(discoveryBody(discoveryFixture(true), "otAQ7b"))
	}}
	a := &CookieAccount{ID: 1, Cookie: "synthetic"}
	if _, err := c.get(context.Background(), a, ""); err != nil {
		t.Fatal(err)
	}
	fail = true
	now = now.Add(modelCatalogTTL)
	if _, err := c.get(context.Background(), a, ""); err == nil {
		t.Fatal("schema/auth failure used stale catalog")
	}
	if _, ok := c.cached(a, ""); ok {
		t.Fatal("invalidated catalog still exposed")
	}
}

func TestAccountModelCatalogLegacyAndCapacity(t *testing.T) {
	p := discoveryFixture(true)
	p[16] = []any{21}
	for _, r := range p[15].([]any) {
		row := r.([]any)
		row[9] = row[17]
		row[17] = nil
	}
	c, err := parseAccountModelCatalog(discoveryBody(p, "otAQ7b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Models {
		if m.Capacity != 1 || m.CapacityField != 13 {
			t.Fatal("capacity field variant lost")
		}
	}
	p[15].([]any)[0].([]any)[17] = "malformed"
	if _, err := parseAccountModelCatalog(discoveryBody(p, "otAQ7b")); err == nil {
		t.Fatal("bad new field fell back to legacy mode")
	}
}

// The leader's deadline/cancellation must not erase previously confirmed
// capabilities or impose a negative-cache period on independent callers.
func TestAccountModelCatalogCanceledLeaderPreservesCache(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			now := time.Unix(100000, 0)
			var calls atomic.Int32
			started := make(chan struct{})
			c := accountCatalogCache{entries: map[string]*catalogCacheEntry{}, now: func() time.Time { return now }, fetch: func(ctx context.Context, _ *CookieAccount, _ string) (AccountModelCatalog, error) {
				if calls.Add(1) == 2 {
					close(started)
					<-ctx.Done()
					return AccountModelCatalog{}, ctx.Err()
				}
				return parseAccountModelCatalog(discoveryBody(discoveryFixture(true), "otAQ7b"))
			}}
			a := &CookieAccount{ID: 1, Cookie: "synthetic"}
			first, err := c.get(context.Background(), a, "")
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(modelCatalogTTL)
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.get(ctx, a, ""); done <- err }()
			<-started
			if !deadline {
				cancel()
			}
			err = <-done
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			saved, ok := c.cached(a, "")
			if !ok || !saved.Stale || !saved.FetchedAt.Equal(first.FetchedAt) {
				t.Fatal("last good catalog discarded or refreshed")
			}
			fresh, err := c.get(context.Background(), a, "")
			if err != nil || fresh.Stale || calls.Load() != 3 {
				t.Fatalf("refresh suppressed: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestCatalogXSRFRecoveryBounded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		recover bool
		cancel  bool
	}{
		{"xsrf", 400, `[["er",null,400,["xsrf"]]]`, true, false},
		{"ordinary400", 400, `bad request`, false, false},
		{"forbidden", 403, `["xsrf"]`, false, false},
		{"cancel", 400, `["xsrf"]`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := &CookieAccount{ID: 1, Cookie: "synthetic", AuthUser: 2}
			gets, posts, invalidates := 0, 0, 0
			token := func(got context.Context, c string, u int, p string) (string, error) {
				gets++
				if got != ctx || c != a.Cookie || u != 2 || p != "egress" {
					t.Fatal("context or account changed")
				}
				if gets == 1 {
					return "old", nil
				}
				return "fresh", nil
			}
			invalidate := func(c string) {
				invalidates++
				if c != a.Cookie {
					t.Fatal("wrong account invalidated")
				}
				if tc.cancel {
					cancel()
				}
			}
			post := func(got context.Context, tok string) (int, []byte, error) {
				posts++
				if got != ctx {
					t.Fatal("detached context")
				}
				if posts == 1 {
					if tok != "old" {
						t.Fatal("wrong token")
					}
					return tc.status, []byte(tc.body), nil
				}
				if tok != "fresh" {
					t.Fatal("stale token reused")
				}
				return 200, discoveryBody(discoveryFixture(true), "otAQ7b"), nil
			}
			_, err := fetchAccountModelsWithRPC(ctx, a, "egress", token, invalidate, post)
			if tc.cancel {
				if !errors.Is(err, context.Canceled) || posts != 1 || gets != 1 {
					t.Fatal("cancellation retried", err, posts, gets)
				}
				return
			}
			if tc.recover {
				if err != nil || posts != 2 || gets != 2 || invalidates != 1 {
					t.Fatal("recovery failed", err, posts, gets, invalidates)
				}
			} else if err == nil || posts != 1 || gets != 1 || invalidates != 0 {
				t.Fatal("unexpected recovery", err, posts, gets, invalidates)
			}
		})
	}
	// Repeated rejection must never become an unbounded retry.
	n := 0
	_, err := fetchAccountModelsWithRPC(context.Background(), &CookieAccount{Cookie: "synthetic"}, "", func(context.Context, string, int, string) (string, error) { return "t", nil }, func(string) {}, func(context.Context, string) (int, []byte, error) { n++; return 400, []byte(`["xsrf"]`), nil })
	if err == nil || n != 2 {
		t.Fatal("unbounded XSRF retry", n, err)
	}
}

func TestCatalogRouteInternalDeadlineFallback(t *testing.T) {
	// No parallel use: replace only the test cache's data/functions, not its mutex.
	oldEntries, oldNow, oldFetch := liveModelCatalogs.entries, liveModelCatalogs.now, liveModelCatalogs.fetch
	defer func() {
		liveModelCatalogs.entries = oldEntries
		liveModelCatalogs.now = oldNow
		liveModelCatalogs.fetch = oldFetch
	}()
	now := time.Unix(100000, 0)
	a := &CookieAccount{ID: 900, Cookie: "synthetic", Status: "enabled"}
	cat, err := parseAccountModelCatalog(discoveryBody(discoveryFixture(true), "otAQ7b"))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := catalogAccountKey(a, "")
	liveModelCatalogs.now = func() time.Time { return now }
	liveModelCatalogs.fetch = func(ctx context.Context, _ *CookieAccount, _ string) (AccountModelCatalog, error) {
		<-ctx.Done()
		return AccountModelCatalog{}, ctx.Err()
	}
	mc := ModelConfig{Upstream: "4 Carbon", RequireModelIdentity: true}
	for _, age := range []time.Duration{6 * time.Minute, 16 * time.Minute} {
		cat.FetchedAt = now.Add(-age)
		liveModelCatalogs.entries = map[string]*catalogCacheEntry{key: {catalog: cat}}
		got, err := accountModelRouteBudget(context.Background(), a, "", mc, time.Millisecond)
		if age < modelCatalogMaxAge {
			if err != nil || got.HexID != "95a4c0abcba24364" {
				t.Fatal("valid stale refused", err)
			}
		} else if err == nil {
			t.Fatal("expired stale accepted")
		}
	}
	cat.FetchedAt = now.Add(-6 * time.Minute)
	liveModelCatalogs.entries = map[string]*catalogCacheEntry{key: {catalog: cat}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := accountModelRouteBudget(ctx, a, "", mc, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation used stale", err)
	}
	liveModelCatalogs.fetch = func(context.Context, *CookieAccount, string) (AccountModelCatalog, error) {
		return AccountModelCatalog{}, errModelCatalog
	}
	if _, err := accountModelRouteBudget(context.Background(), a, "", mc, time.Millisecond); err == nil {
		t.Fatal("authoritative rejection used stale")
	}
}
