package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type ModelAvailabilityError struct {
	Code   string
	Status int
}

func (e *ModelAvailabilityError) Error() string { return e.Code }

func modelRequestContext(mc ModelConfig) context.Context {
	if mc.RequestContext == nil {
		return context.Background()
	}
	return mc.RequestContext
}

// Set phase budgets only for generation. Catalog and prepare calls use their
// own bounded caller context. An explicit test/worker override is preserved.
type generationLatencyKey struct{}

func modelGenerationContext(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, generationLatencyKey{}, true)
	if _, ok := ctx.Value(generationTimeoutsKey{}).(generationTimeouts); ok {
		return ctx
	}
	cfg := rtCfg()
	return withGenerationTimeouts(ctx, generationTimeouts{
		Overall:    time.Duration(cfg.RequestTimeout) * time.Second,
		FirstFrame: time.Duration(cfg.FirstFrameTimeoutSec) * time.Second,
		Idle:       time.Duration(cfg.StreamIdleTimeoutSec) * time.Second,
	})
}

func mergeAccountExclusions(a, b map[int64]bool) map[int64]bool {
	out := make(map[int64]bool, len(a)+len(b))
	for k, v := range a {
		if v {
			out[k] = true
		}
	}
	for k, v := range b {
		if v {
			out[k] = true
		}
	}
	return out
}

func strongerModelAvailability(old, next error) error {
	var a *ModelAvailabilityError
	if errors.As(old, &a) && a.Status == http.StatusServiceUnavailable {
		return old
	}
	return next
}

func canonicalCapability(mc ModelConfig) string {
	switch mc.Upstream {
	case "4 Hydrogen":
		return "gemini-4-hydrogen"
	case "4 Carbon":
		return "gemini-4-carbon"
	case "4 Argon":
		return "gemini-4-argon"
	case "3.1 Pro":
		return "gemini-3.1-pro"
	default:
		return ""
	}
}

func routeFromCatalog(cat AccountModelCatalog, mc ModelConfig) (ModelConfig, bool) {
	if !mc.RequireModelIdentity {
		return mc, true
	}
	entry, ok := cat.Lookup(canonicalCapability(mc))
	if !ok || entry.HexID == "" || entry.Mode <= 0 {
		return mc, false
	}
	// Normal is the base catalog route. Extended/medium are separate and must
	// have policy evidence on that same route, never a policy-only model.
	if mc.thinkWire() != thinkingNormal && !entry.SupportsThinking(mc.thinkWire()) {
		return mc, false
	}
	mc.HexID, mc.Mode = entry.HexID, entry.Mode
	mc.CatalogCapacity, mc.CatalogCapacityField = entry.Capacity, entry.CapacityField
	return mc, true
}

func accountModelRoute(ctx context.Context, account *CookieAccount, proxy string, mc ModelConfig) (ModelConfig, error) {
	return accountModelRouteBudget(ctx, account, proxy, mc, 8*time.Second)
}

func accountModelRouteBudget(ctx context.Context, account *CookieAccount, proxy string, mc ModelConfig, budget time.Duration) (ModelConfig, error) {
	if !mc.RequireModelIdentity {
		return mc, nil
	}
	if ctx.Err() != nil {
		return mc, ctx.Err()
	}
	if account == nil || account.Status != "enabled" {
		return mc, &ModelAvailabilityError{Code: "model_unavailable", Status: http.StatusNotFound}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	markRequestLatency(ctx, latencyCatalogStart)
	cat, err := discoverAccountModels(fetchCtx, account, proxy)
	markRequestLatency(ctx, latencyCatalogEnd)
	if ctx.Err() != nil {
		return mc, ctx.Err()
	}
	// Only an internal deadline may fall back, never parent cancellation or
	// an authoritative schema/auth rejection. cachedAccountModels enforces max age.
	if err != nil && errors.Is(err, context.DeadlineExceeded) && fetchCtx.Err() == context.DeadlineExceeded {
		if saved, ok := cachedAccountModels(account, proxy); ok {
			cat, err = saved, nil
		}
	}
	if err != nil {
		return mc, &ModelAvailabilityError{Code: "model_catalog_unavailable", Status: http.StatusServiceUnavailable}
	}
	route, ok := routeFromCatalog(cat, mc)
	if !ok {
		return mc, &ModelAvailabilityError{Code: "model_unavailable", Status: http.StatusNotFound}
	}
	return route, nil
}

// Public catalog advertises only models confirmed on at least one enabled
// account. Work is bounded even on cold start; canceled discovery can't expand
// capabilities. Static anonymous/media entries keep their existing semantics.
func catalogModels(ctx context.Context) map[string]ModelConfig {
	base := availableModels()
	out := make(map[string]ModelConfig, len(base))
	for name, mc := range base {
		if !mc.RequireModelIdentity {
			out[name] = mc
		}
	}
	listCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	jobs := make(chan CookieAccount)
	for range 4 {
		wg.Go(func() {
			for account := range jobs {
				if listCtx.Err() != nil {
					return
				}
				proxy, ok := catalogProxyForAccount(account.ProxyID)
				if !ok {
					continue
				}
				cat, err := discoverAccountModels(listCtx, &account, proxy)
				if err != nil {
					continue
				}
				mu.Lock()
				for name, mc := range Models {
					if !mc.RequireModelIdentity || isLegacyFlashAlias(name) {
						continue
					}
					if _, ok := routeFromCatalog(cat, mc); ok {
						out[name] = mc
					}
				}
				mu.Unlock()
			}
		})
	}
send:
	for _, a := range accountList() {
		if a.Status != "enabled" {
			continue
		}
		select {
		case jobs <- a:
		case <-listCtx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()
	return out
}

func catalogProxyForAccount(preferred int64) (string, bool) {
	proxies := listProxies()
	now := time.Now().Unix()
	cooldown := rtCfg().ProxyCooldownMin
	var candidate string
	for _, p := range proxies {
		if !proxyUsable(p, now, cooldown) {
			continue
		}
		if p.ID == preferred {
			return p.URL, true
		}
		if candidate == "" {
			candidate = p.URL
		}
	}
	if candidate != "" {
		return candidate, true
	}
	return "", len(proxies) == 0 || rtCfg().FallbackDirect
}

func isLegacyFlashAlias(name string) bool {
	return strings.HasPrefix(name, "gemini-3.7-flash") || strings.HasPrefix(name, "gemini-3.8-flash")
}

func modelMismatchForAccount(err error, account int64) error {
	var identity *ModelIdentityError
	if errors.As(err, &identity) && foldModelName(identity.Reported) == "3.5 flash lite" {
		// The old per-account cooldown/switch path applies to a definitive
		// downgrade only, never to timeout, unknown identity, or a slot rename.
		return &ModelDowngradedError{Requested: identity.Requested, Reported: identity.Reported, AccountID: account}
	}
	return err
}

func modelRouteHeader(mc ModelConfig, uid string) string {
	if mc.CatalogCapacity <= 0 {
		return buildModelHeader(mc.HexID, mc.Mode, mc.thinkWire(), uid)
	}
	if mc.CatalogCapacityField == 13 {
		return fmt.Sprintf(`[1,null,null,null,"%s",null,null,0,[4,5,6,8],null,null,null,%d,null,null,%d,%d,"%s"]`, mc.HexID, mc.CatalogCapacity, mc.Mode, mc.thinkWire(), uid)
	}
	return fmt.Sprintf(`[1,null,null,null,"%s",null,null,0,[4,5,6,8],null,null,%d,null,null,%d,%d,"%s"]`, mc.HexID, mc.CatalogCapacity, mc.Mode, mc.thinkWire(), uid)
}

var publicModelSyntax = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,79}(@think=[0-9]{1,4})?$`)

func safeRequestedModel(requested, canonical string) string {
	if publicModelSyntax.MatchString(requested) {
		return requested
	}
	requested, _, _ = strings.Cut(requested, "@think=")
	if publicModelSyntax.MatchString(requested) {
		return requested
	}
	return canonical
}

var loggedModelLabel = regexp.MustCompile(`^[2-9](\.[0-9]+)? [\pL\pN]+([ -][\pL\pN]+){0,4}$`)

func safeReportedModel(name string) string {
	if len(name) <= 60 && loggedModelLabel.MatchString(name) {
		return name
	}
	return ""
}
