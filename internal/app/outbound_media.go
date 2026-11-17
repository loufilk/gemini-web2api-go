package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const mediaIOTimeout = 90 * time.Second
const maxArtifactBytes = 100 << 20

// Never return a silently truncated file. One extra byte distinguishes exact-limit
// content from an oversized response, including chunked/decompressed responses.
func readMediaBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("media response exceeds %d-byte limit", limit)
	}
	return b, nil
}

func trustedMediaURL(raw string, upload bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") {
		return fmt.Errorf("untrusted media destination")
	}
	host := strings.ToLower(u.Hostname())
	allowed := host == "lh3.googleusercontent.com" || host == "work.fife.usercontent.google.com" || host == "contribution.usercontent.google.com"
	if upload {
		allowed = host == "push.clients6.google.com" || host == "content-push.googleapis.com"
	}
	if !allowed {
		return fmt.Errorf("untrusted media destination")
	}
	return nil
}

// Exclude special-use ranges as well as RFC1918/link-local/loopback. IPv6
// transition addresses are excluded to prevent embedded non-public IPv4 routing.
var nonPublicMediaPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicMediaIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	for _, prefix := range nonPublicMediaPrefixes {
		if prefix.Contains(a) {
			return false
		}
	}
	return true
}

func userMediaURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid remote media URL")
	}
	if u.Port() != "" && !((u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80")) {
		return fmt.Errorf("remote media requires the default HTTP(S) port")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !publicMediaIP(ip) {
		return fmt.Errorf("remote media requires a public destination")
	}
	return nil
}

// Untrusted user URLs use a dedicated cookie-free transport. Resolve and pin the
// validated address in DialContext, not in a separate preflight DNS lookup.
// A remote proxy can resolve differently and cannot enforce this boundary.
func newUserMediaClient() *http.Client {
	tr := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DisableKeepAlives: true}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid media address")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("media host lookup failed")
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("media host has no addresses")
		}
		for _, ip := range ips {
			if !publicMediaIP(ip.IP) {
				return nil, fmt.Errorf("remote media requires a public destination")
			}
		}
		for _, ip := range ips {
			c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return c, nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("media connection failed")
	}
	return &http.Client{Transport: tr, Timeout: mediaIOTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 6 {
			return fmt.Errorf("too many media redirects")
		}
		return userMediaURL(req.URL.String())
	}}
}

// net/http errors include the complete signed URL and proxy address. Keep only
// cancellation semantics, never expose those credentials through API/log errors.
func mediaTransportError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("media transport failed")
}

func trustedGoogleRPCURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() != "gemini.google.com" || (u.Port() != "" && u.Port() != "443") || !strings.HasSuffix(u.Path, "/_/BardChatUi/data/batchexecute") {
		return fmt.Errorf("untrusted Google RPC destination")
	}
	return nil
}

func waitMediaContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
