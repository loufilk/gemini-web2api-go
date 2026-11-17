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
