package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMediaDestinationPolicy(t *testing.T) {
	for _, u := range []string{"https://lh3.googleusercontent.com/gg/image", "https://work.fife.usercontent.google.com/file", "https://contribution.usercontent.google.com/download"} {
		if err := trustedMediaURL(u, false); err != nil {
			t.Errorf("known download host rejected: %v", err)
		}
	}
	for _, u := range []string{"http://lh3.googleusercontent.com/file", "https://lh3.googleusercontent.com.example.org/file", "https://user:pass@lh3.googleusercontent.com/file", "https://lh3.googleusercontent.com:8443/file", "https://example.org/file", "https://127.0.0.1/file"} {
		if trustedMediaURL(u, false) == nil {
			t.Error("untrusted media URL accepted")
		}
	}
	for _, u := range []string{"https://push.clients6.google.com/upload/", "https://content-push.googleapis.com/upload/"} {
		if trustedMediaURL(u, true) != nil {
			t.Error("known upload host rejected")
		}
	}
	if trustedMediaURL("https://lh3.googleusercontent.com/upload", true) == nil {
		t.Error("download host allowed upload credentials")
	}
}

func TestMediaPublicDestinationPolicy(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "198.18.0.1", "192.0.2.1", "240.1.1.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1", "2001:db8::1"} {
		if publicMediaIP(net.ParseIP(ip)) {
			t.Errorf("special-use address allowed: %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicMediaIP(net.ParseIP(ip)) {
			t.Errorf("public address rejected: %s", ip)
		}
	}
	for _, u := range []string{"https://example.org:8080/file", "http://user:pass@example.org/file", "file:///tmp/image", "http://127.0.0.1/file"} {
		if userMediaURL(u) == nil {
			t.Error("invalid remote URL accepted")
		}
	}
}

func TestMediaBodyLimitDoesNotTruncate(t *testing.T) {
	if b, err := readMediaBody(strings.NewReader("abcd"), 4); err != nil || string(b) != "abcd" {
		t.Fatal("exact limit rejected", err)
	}
	if b, err := readMediaBody(strings.NewReader("abcde"), 4); err == nil || b != nil {
		t.Fatal("over-limit body returned success or truncated bytes")
	}
	// Regression: remote video previously silently stopped at the image limit.
	data := bytes.Repeat([]byte{'v'}, maxImageBytes+1024)
	got, err := materializeMediaBody(bytes.NewReader(data), "video/mp4", 1)
	if err != nil || len(got.Data) != len(data) || got.Kind != 2 {
		t.Fatalf("video truncated: %d %v", len(got.Data), err)
	}
	if _, err := materializeMediaBody(bytes.NewReader(data), "image/png", 1); err == nil {
		t.Fatal("oversized image accepted")
	}
}

type mediaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mediaRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMediaRedirectRejectsUntrustedBeforeSending(t *testing.T) {
	proxy := "http://media-policy-test.invalid:80"
	key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
	calls := 0
	stdlibClientCache.Store(key, &http.Client{Transport: mediaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Hostname() != "lh3.googleusercontent.com" {
			t.Fatal("untrusted destination reached transport")
		}
		if r.Header.Get("Cookie") != "test=value" {
			t.Error("trusted destination did not receive allowed cookie")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://example.org/file"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	defer stdlibClientCache.Delete(key)
	if _, _, _, err := mediaGetFollow("https://lh3.googleusercontent.com/gg/file", map[string]string{"Cookie": "test=value"}, proxy); err == nil {
		t.Fatal("untrusted redirect accepted")
	}
	if calls != 1 {
		t.Fatalf("transport calls %d, want 1", calls)
	}
}

func TestMediaCancellationAndUploadTargetRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := materializeImageContext(ctx, "data:image/png;base64,eA==", "", 1); err != context.Canceled {
		t.Fatalf("attachment ignored cancellation: %v", err)
	}
	if _, err := uploadBytesContext(ctx, "", 0, "", []byte("x"), "file"); err == nil {
		t.Fatal("upload ignored cancellation")
	}
	if _, _, _, err := uploadPostContext(context.Background(), "https://example.org/upload", map[string]string{"Cookie": "test=value"}, []byte("x"), ""); err == nil {
		t.Fatal("untrusted upload destination accepted")
	}
}

func TestMediaRequestContextReachesTransport(t *testing.T) {
	proxy := "http://media-context-test.invalid:80"
	key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := make(chan struct{})
	stdlibClientCache.Store(key, &http.Client{Transport: mediaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(reached)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})})
	defer stdlibClientCache.Delete(key)
	done := make(chan error, 1)
	go func() {
		_, _, _, err := mediaGetOnceContext(ctx, "https://lh3.googleusercontent.com/gg/file?token=private", nil, proxy)
		done <- err
	}()
	<-reached
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestMediaGoogleRPCTransportRemainsAllowed(t *testing.T) {
	proxy := "http://media-rpc-test.invalid:80"
	key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
	calls := 0
	stdlibClientCache.Store(key, &http.Client{Transport: mediaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Hostname() != "gemini.google.com" {
			t.Fatal("wrong RPC host")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]")), Request: r}, nil
	})})
	defer stdlibClientCache.Delete(key)
	for _, rpc := range []string{"hNvQHb", "c8o8Fe", "GzXR5e"} {
		if _, _, _, err := uploadPost("https://gemini.google.com/u/1/_/BardChatUi/data/batchexecute?rpcids="+rpc, nil, nil, proxy); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatal("expected all supported RPC requests")
	}
	// Upload finalization is a separate trust boundary, not the generic RPC path.
	if _, _, _, err := uploadPostContext(context.Background(), "https://gemini.google.com/_/BardChatUi/data/batchexecute", nil, nil, proxy); err == nil {
		t.Fatal("upload finalization accepted an RPC URL")
	}
}

func TestMediaArtifactCancellationBeforePolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tool := range []int{toolImage, toolMusic, toolVideo} {
		if _, _, err := fetchMediaArtifactsContext(ctx, tool, "", "c_test", "r_test", "", "", "", "", "", 0); err != context.Canceled {
			t.Fatalf("tool %d ignored cancellation: %v", tool, err)
		}
	}
	if err := waitMediaContext(ctx, time.Hour); err != context.Canceled {
		t.Fatal("poll timer ignored cancellation")
	}
}
