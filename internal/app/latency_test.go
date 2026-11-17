package app

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRequestLatencyMissingAndZero(t *testing.T) {
	absent := snapshotRequestLatency(context.Background())
	if !absent.Started.IsZero() || absent.Submitted != nil {
		t.Fatalf("absent tracker: %+v", absent)
	}
	ctx := withRequestLatency(context.Background())
	r := ctx.Value(requestLatencyKey{}).(*requestLatency)
	r.events[latencySubmitted].Store(0)
	got := snapshotRequestLatency(ctx)
	if got.Submitted == nil || *got.Submitted != 0 || got.UpstreamText != nil {
		t.Fatalf("zero/missing conflated: %+v", got)
	}
}
func TestRequestLatencyFirstEventWins(t *testing.T) {
	ctx := withRequestLatency(context.Background())
	markRequestLatency(ctx, latencyCatalogStart)
	first := *snapshotRequestLatency(ctx).CatalogStart
	for i := 0; i < 100; i++ {
		markRequestLatency(ctx, latencyCatalogStart)
	}
	if got := *snapshotRequestLatency(ctx).CatalogStart; got != first {
		t.Fatalf("event overwritten: %v -> %v", first, got)
	}
	if withRequestLatency(ctx) != ctx {
		t.Fatal("rewrap reset origin")
	}
	markRequestLatency(context.Background(), latencySubmitted)
	markRequestLatency(ctx, latencyEvent(255))
}
func TestRequestLatencyConcurrentMarksAndSnapshots(t *testing.T) {
	ctx := withRequestLatency(context.Background())
	var wg sync.WaitGroup
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := latencyEvent(0); e < latencyEventCount; e++ {
				markRequestLatency(ctx, e)
				_ = snapshotRequestLatency(ctx)
			}
		}()
	}
	wg.Wait()
	got := snapshotRequestLatency(ctx)
	values := []*time.Duration{got.CatalogStart, got.CatalogEnd, got.Submitted, got.UpstreamText, got.Reasoning, got.ModelVerified, got.DownstreamText, got.Tool, got.Heartbeat}
	for i, v := range values {
		if v == nil || *v < 0 || *v > got.Elapsed {
			t.Fatalf("invalid event %d: %v", i, v)
		}
	}
	// Snapshots own their values rather than exposing mutable tracker storage.
	*got.Submitted = -123
	if *snapshotRequestLatency(ctx).Submitted < 0 {
		t.Fatal("snapshot mutated tracker")
	}
}
func TestRequestLatencyChildAndCanceledContext(t *testing.T) {
	parent := withRequestLatency(context.Background())
	child, cancel := context.WithCancel(parent)
	cancel()
	markRequestLatency(child, latencyHeartbeat)
	got := snapshotRequestLatency(parent)
	if got.Heartbeat == nil || got.UpstreamText != nil || got.DownstreamText != nil {
		t.Fatalf("heartbeat conflated with content: %+v", got)
	}
}
func BenchmarkRequestLatencyDuplicateMark(b *testing.B) {
	ctx := withRequestLatency(context.Background())
	markRequestLatency(ctx, latencySubmitted)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		markRequestLatency(ctx, latencySubmitted)
	}
}

func TestRequestLatencyPromptMetadata(t *testing.T) {
	ctx := withRequestLatency(context.Background())
	missing := snapshotRequestLatency(ctx)
	if missing.PromptBytes != nil || missing.PromptBudgetBytes != nil || missing.ContextAttachmentUsed != nil {
		t.Fatal("missing metadata is not nil")
	}
	noteRequestPrompt(ctx, -1, 10)
	if snapshotRequestLatency(ctx).PromptBytes != nil {
		t.Fatal("invalid count accepted")
	}
	noteRequestPrompt(ctx, 0, 100)
	noteRequestPrompt(ctx, 200, 300)
	noteContextAttachment(ctx, false)
	noteContextAttachment(ctx, true)
	markRequestLatency(ctx, latencyPrepareStart)
	markRequestLatency(ctx, latencyPrepareEnd)
	got := snapshotRequestLatency(ctx)
	if got.PromptBytes == nil || *got.PromptBytes != 0 || got.PromptBudgetBytes == nil || *got.PromptBudgetBytes != 100 || got.ContextAttachmentUsed == nil || *got.ContextAttachmentUsed || got.PrepareStart == nil || got.PrepareEnd == nil {
		t.Fatalf("incorrect metadata: %+v", got)
	}
	*got.PromptBytes = 999
	*got.ContextAttachmentUsed = true
	again := snapshotRequestLatency(ctx)
	if *again.PromptBytes != 0 || *again.ContextAttachmentUsed {
		t.Fatal("snapshot mutated metadata")
	}
}
func TestRequestLatencyConcurrentMetadata(t *testing.T) {
	ctx := withRequestLatency(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			noteRequestPrompt(ctx, n, n+100)
			noteContextAttachment(ctx, n%2 == 0)
			_ = snapshotRequestLatency(ctx)
		}(i)
	}
	wg.Wait()
	got := snapshotRequestLatency(ctx)
	if got.PromptBytes == nil || got.PromptBudgetBytes == nil || *got.PromptBudgetBytes-*got.PromptBytes != 100 || got.ContextAttachmentUsed == nil {
		t.Fatalf("torn metadata pair: %+v", got)
	}
}
