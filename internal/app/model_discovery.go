package app

// Registry schema pinned to gemini-stealth-bridge commit
// ace32031a4ba9576d9f1d913c996e03d9bb6d087, client.go AvailableModels and
// models_meta_test.go. These are schema evidence, not an availability guarantee.
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const modelCatalogTTL = 5 * time.Minute
const modelCatalogMaxAge = 15 * time.Minute
const modelCatalogRetry = 30 * time.Second
const modelCatalogLimit = 256

var errModelCatalog = errors.New("account model catalog unavailable or unrecognized")
var errModelCatalogTransient = errors.New("account model catalog temporarily unavailable")

type DiscoveredModel struct {
	HexID                   string
	CanonicalName           string // versioned API id, never a compatibility hex or UI phrase
	DisplayName             string // versioned upstream product identity
	Mode                    int
	ThinkingModes           []int
	ThinkingPolicyKnown     bool
	Capacity, CapacityField int
}

func (m DiscoveredModel) SupportsThinking(mode int) bool {
	if !m.ThinkingPolicyKnown {
		return false
	}
	for _, v := range m.ThinkingModes {
		if v == mode {
			return true
		}
	}
	return false
}

type AccountModelCatalog struct {
	Models    []DiscoveredModel
	FetchedAt time.Time
	Stale     bool
}

func (c AccountModelCatalog) Lookup(canonical string) (DiscoveredModel, bool) {
	for _, m := range c.Models {
		if m.CanonicalName == canonical {
			m.ThinkingModes = append([]int(nil), m.ThinkingModes...)
			return m, true
		}
	}
	return DiscoveredModel{}, false
}
func cloneModelCatalog(c AccountModelCatalog) AccountModelCatalog {
	c.Models = append([]DiscoveredModel(nil), c.Models...)
	for i := range c.Models {
		c.Models[i].ThinkingModes = append([]int(nil), c.Models[i].ThinkingModes...)
	}
	return c
}

type catalogCacheEntry struct {
	catalog AccountModelCatalog
	retryAt time.Time
	loading chan struct{}
}
type accountCatalogCache struct {
	mu      sync.Mutex
	entries map[string]*catalogCacheEntry
	now     func() time.Time
	fetch   func(context.Context, *CookieAccount, string) (AccountModelCatalog, error)
}

var liveModelCatalogs = accountCatalogCache{entries: make(map[string]*catalogCacheEntry), now: time.Now, fetch: fetchAccountModels}

func catalogAccountKey(a *CookieAccount, proxy string) (string, error) {
	if a == nil || a.ID <= 0 || a.Cookie == "" || a.AuthUser < 0 {
		return "", errModelCatalog
	}
	// Credential rotation, account slot and egress changes invalidate the entry.
	return fmt.Sprintf("%d/%d/%x/%x", a.ID, a.AuthUser, catalogIdentityDigest(a.Cookie), sha256.Sum256([]byte(proxy))), nil
}

// Hash identity-bearing cookies, ignoring routine SIDCC/session refresh tokens.
// Retain no raw credential in the key. Unknown cookie formats use the full digest.
func catalogIdentityDigest(cookie string) [32]byte {
	var pairs []string
	for _, p := range splitCookiePairs(cookie) {
		switch p[0] {
		case "SID", "__Secure-1PSID", "__Secure-3PSID", "SAPISID", "APISID", "__Secure-1PAPISID", "__Secure-3PAPISID":
			pairs = append(pairs, p[0]+"="+p[1])
		}
	}
	if len(pairs) == 0 {
		return sha256.Sum256([]byte(cookie))
	}
	sort.Strings(pairs)
	return sha256.Sum256([]byte(strings.Join(pairs, "\x00")))
}
func cachedAccountModels(a *CookieAccount, proxy string) (AccountModelCatalog, bool) {
	return liveModelCatalogs.cached(a, proxy)
}
func (c *accountCatalogCache) cached(a *CookieAccount, proxy string) (AccountModelCatalog, bool) {
	key, err := catalogAccountKey(a, proxy)
	if err != nil {
		return AccountModelCatalog{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || e.catalog.FetchedAt.IsZero() || c.now().Sub(e.catalog.FetchedAt) >= modelCatalogMaxAge {
		return AccountModelCatalog{}, false
	}
	out := cloneModelCatalog(e.catalog)
	out.Stale = c.now().Sub(out.FetchedAt) >= modelCatalogTTL
	return out, true
}
func discoverAccountModels(ctx context.Context, a *CookieAccount, proxy string) (AccountModelCatalog, error) {
	return liveModelCatalogs.get(ctx, a, proxy)
}
func (c *accountCatalogCache) get(ctx context.Context, a *CookieAccount, proxy string) (AccountModelCatalog, error) {
	key, err := catalogAccountKey(a, proxy)
	if err != nil {
		return AccountModelCatalog{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return AccountModelCatalog{}, err
		}
		c.mu.Lock()
		now := c.now()
		e := c.entries[key]
		if e != nil && !e.catalog.FetchedAt.IsZero() && now.Sub(e.catalog.FetchedAt) < modelCatalogTTL {
			out := cloneModelCatalog(e.catalog)
			c.mu.Unlock()
			return out, nil
		}
		if e != nil && e.loading != nil {
			done := e.loading
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return AccountModelCatalog{}, ctx.Err()
			case <-done:
				continue
			}
		}
		if e != nil && now.Before(e.retryAt) {
			c.mu.Unlock()
			if old, ok := c.cached(a, proxy); ok {
				return old, nil
			}
			return AccountModelCatalog{}, errModelCatalog
		}
		if e == nil {
			if len(c.entries) >= modelCatalogLimit {
				for k, v := range c.entries {
					if v.loading == nil {
						delete(c.entries, k)
						if len(c.entries) < modelCatalogLimit {
							break
						}
					}
				}
			}
			if len(c.entries) >= modelCatalogLimit {
				c.mu.Unlock()
				return AccountModelCatalog{}, errModelCatalog
			}
			e = &catalogCacheEntry{}
			c.entries[key] = e
		}
		e.loading = make(chan struct{})
		done := e.loading
		c.mu.Unlock()
		// Snapshot account: concurrent caller updates must not mutate fetch parameters.
		account := *a
		result, fetchErr := c.fetch(ctx, &account, proxy)
		c.mu.Lock()
		now = c.now()
		// A caller's lifetime is not account capability evidence. Do not commit
		// late results or poison the shared cache when the refresh leader exits.
		// Waiters retain their own contexts and may immediately become leader.
		canceled := ctx.Err() != nil || errors.Is(fetchErr, context.Canceled) || errors.Is(fetchErr, context.DeadlineExceeded)
		if canceled {
			e.retryAt = time.Time{}
		} else if fetchErr == nil && len(result.Models) > 0 {
			result.FetchedAt = now
			result.Stale = false
			e.catalog = cloneModelCatalog(result)
			e.retryAt = time.Time{}
		} else {
			e.retryAt = now.Add(modelCatalogRetry)
			if !errors.Is(fetchErr, errModelCatalogTransient) {
				e.catalog = AccountModelCatalog{}
			}
			if fetchErr == nil {
				fetchErr = errModelCatalog
			}
		}
		e.loading = nil
		close(done)
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return AccountModelCatalog{}, err
		}
		if canceled {
			return AccountModelCatalog{}, fetchErr
		}
		if fetchErr == nil {
			return cloneModelCatalog(result), nil
		}
		if old, ok := c.cached(a, proxy); ok {
			return old, nil
		}
		return AccountModelCatalog{}, errModelCatalog
	}
}

// Discovery shares the selected account authentication, egress and cancellation.
// No errors include upstream response bodies, cookies, or proxy credentials.
func fetchAccountModels(ctx context.Context, a *CookieAccount, proxy string) (AccountModelCatalog, error) {
	return fetchAccountModelsWithRPC(ctx, a, proxy, getXSRFContext, invalidateXSRF, func(ctx context.Context, token string) (int, []byte, error) {
		req, _ := json.Marshal([]any{[]any{[]any{"otAQ7b", "[]", nil, "generic"}}})
		form := url.Values{"f.req": {string(req)}, "at": {token}}
		ep := fmt.Sprintf("%s/_/BardChatUi/data/batchexecute?rpcids=otAQ7b&bl=%s&hl=en&_reqid=%d&rt=c", geminiBase(a.AuthUser), url.QueryEscape(currentBL(proxy)), time.Now().UnixNano()%1000000)
		h := buildGeminiHeaders(a.Cookie, extractSAPISID(a.Cookie), "")
		setAuthUserHeaders(h, a.AuthUser)
		size := 0
		status, body, _, _, err := doGeminiRequestContext(ctx, ep, form.Encode(), h, proxy, func(line string) error {
			size += len(line)
			if size > 2<<20 {
				return errModelCatalog
			}
			return nil
		})
		return status, body, err
	})
}

// Dependencies are explicit for offline tests; recovery is limited to this
// read-only RPC, the same account/egress/context, and one proven XSRF rejection.
func fetchAccountModelsWithRPC(ctx context.Context, a *CookieAccount, proxy string,
	token func(context.Context, string, int, string) (string, error), invalidate func(string),
	post func(context.Context, string) (int, []byte, error)) (AccountModelCatalog, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return AccountModelCatalog{}, err
		}
		xsrf, err := token(ctx, a.Cookie, a.AuthUser, proxy)
		if ctx.Err() != nil {
			return AccountModelCatalog{}, ctx.Err()
		}
		if err != nil {
			return AccountModelCatalog{}, errModelCatalog
		}
		status, body, err := post(ctx, xsrf)
		if ctx.Err() != nil {
			return AccountModelCatalog{}, ctx.Err()
		}
		if err == nil && status == 400 && attempt == 0 && isXSRFError(string(body)) {
			invalidate(a.Cookie)
			continue
		}
		if err != nil || status == 429 || status >= 500 {
			return AccountModelCatalog{}, errModelCatalogTransient
		}
		if status != 200 {
			return AccountModelCatalog{}, errModelCatalog
		}
		return parseAccountModelCatalog(body)
	}
	return AccountModelCatalog{}, errModelCatalog
}

var catalogHexRE = regexp.MustCompile(`^[0-9a-f]{16}$`)
var catalogProductRE = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)? [A-Za-z][A-Za-z0-9 -]*$`)

func catalogInt(v any) (int, bool) {
	n, ok := v.(float64)
	if !ok || n < 1 || n > 10000 || n != float64(int(n)) {
		return 0, false
	}
	return int(n), true
}
func catalogString(row []any, slots ...int) string {
	for _, i := range slots {
		if i < len(row) {
			if s, ok := row[i].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}
func catalogAt(row []any, i int) any {
	if i < len(row) {
		return row[i]
	}
	return nil
}
func parseAccountModelCatalog(body []byte) (AccountModelCatalog, error) {
	if len(body) > 2<<20 {
		return AccountModelCatalog{}, errModelCatalog
	}
	var payload []any
	found := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var rows [][]any
		if json.Unmarshal([]byte(line), &rows) != nil {
			return AccountModelCatalog{}, errModelCatalog
		}
		for _, row := range rows {
			if len(row) < 3 || row[0] != "wrb.fr" || row[1] != "otAQ7b" {
				continue
			}
			if found {
				return AccountModelCatalog{}, errModelCatalog
			}
			s, ok := row[2].(string)
			if !ok || json.Unmarshal([]byte(s), &payload) != nil {
				return AccountModelCatalog{}, errModelCatalog
			}
			found = true
		}
	}
	if !found || len(payload) < 16 {
		return AccountModelCatalog{}, errModelCatalog
	}
	rows, ok := payload[15].([]any)
	if !ok || len(rows) == 0 || len(rows) > 256 {
		return AccountModelCatalog{}, errModelCatalog
	}
	levels := map[int][]int{}
	policyKnown := false
	if p := catalogAt(payload, 24); p != nil {
		pol, ok := p.([]any)
		if !ok || len(pol) < 2 {
			return AccountModelCatalog{}, errModelCatalog
		}
		entries, ok := pol[1].([]any)
		if !ok {
			return AccountModelCatalog{}, errModelCatalog
		}
		policyKnown = true
		for _, v := range entries {
			r, ok := v.([]any)
			if !ok || len(r) < 2 {
				return AccountModelCatalog{}, errModelCatalog
			}
			desc, ok := r[0].([]any)
			if !ok || len(desc) < 2 {
				return AccountModelCatalog{}, errModelCatalog
			}
			mode, ok := catalogInt(desc[1])
			if !ok {
				return AccountModelCatalog{}, errModelCatalog
			}
			if _, dup := levels[mode]; dup {
				return AccountModelCatalog{}, errModelCatalog
			}
			lv, ok := r[1].([]any)
			if !ok {
				return AccountModelCatalog{}, errModelCatalog
			}
			levels[mode] = []int{}
			seen := map[int]bool{}
			for _, x := range lv {
				lr, ok := x.([]any)
				if !ok || len(lr) == 0 {
					return AccountModelCatalog{}, errModelCatalog
				}
				n, ok := catalogInt(lr[0])
				if !ok {
					return AccountModelCatalog{}, errModelCatalog
				}
				if (n == 1 || n == 2 || n == 4) && !seen[n] {
					levels[mode] = append(levels[mode], n)
					seen[n] = true
				}
			}
		}
	}
	flags := func(i int) (map[int]bool, error) {
		out := map[int]bool{}
		v := catalogAt(payload, i)
		if v == nil {
			return out, nil
		}
		a, ok := v.([]any)
		if !ok {
			return nil, errModelCatalog
		}
		for _, v := range a {
			n, ok := catalogInt(v)
			if !ok {
				return nil, errModelCatalog
			}
			out[n] = true
		}
		return out, nil
	}
	tier, err := flags(16)
	if err != nil {
		return AccountModelCatalog{}, err
	}
	caps, err := flags(17)
	if err != nil {
		return AccountModelCatalog{}, err
	}
	capacity, field := 1, 12
	switch {
	case tier[21]:
		capacity, field = 1, 13
	case tier[22]:
		capacity, field = 2, 13
	case caps[115]:
		capacity = 4
	case tier[16] || caps[106]:
		capacity = 3
	case tier[8] || caps[19]:
		capacity = 2
	}
	out := AccountModelCatalog{}
	seenHex := map[string]bool{}
	seenName := map[string]bool{}
	for _, v := range rows {
		row, ok := v.([]any)
		if !ok || len(row) < 10 {
			return AccountModelCatalog{}, errModelCatalog
		}
		hex := catalogString(row, 0)
		mode, ok := catalogInt(catalogAt(row, 17))
		if catalogAt(row, 17) == nil {
			mode, ok = catalogInt(catalogAt(row, 9))
		}
		if !ok || !catalogHexRE.MatchString(hex) || seenHex[hex] {
			return AccountModelCatalog{}, errModelCatalog
		}
		seenHex[hex] = true
		// Product description, not marketing label, names the model. No inference
		// from hex: a reused hex is exactly what caused Argon/Carbon mislabelling.
		product := ""
		for _, i := range []int{12, 2, 1, 10, 11, 19} {
			s := catalogString(row, i)
			if catalogProductRE.MatchString(s) {
				product = s
				break
			}
		}
		if product == "" {
			continue
		} // Unknown UI-only entries are never routable.
		name := "gemini-" + strings.Join(strings.Fields(strings.ToLower(product)), "-")
		if seenName[name] {
			return AccountModelCatalog{}, errModelCatalog
		}
		seenName[name] = true
		out.Models = append(out.Models, DiscoveredModel{HexID: hex, CanonicalName: name, DisplayName: product, Mode: mode, ThinkingModes: levels[mode], ThinkingPolicyKnown: policyKnown, Capacity: capacity, CapacityField: field})
	}
	if len(out.Models) == 0 {
		return AccountModelCatalog{}, errModelCatalog
	}
	return out, nil
}
