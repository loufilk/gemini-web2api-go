package app

import (
	"context"
	"sync/atomic"
	"time"
)

// Events are observations, not assertions that an upstream accepted work or that
// a remote client received bytes. Submitted means the generation send was begun;
// downstream events should be marked only after a successful local write.
type latencyEvent uint8

const (
	latencyCatalogStart latencyEvent = iota
	latencyCatalogEnd
	latencyPrepareStart
	latencyPrepareEnd
	latencySubmitted
	latencyUpstreamText
	latencyReasoning
	latencyModelVerified
	latencyDownstreamText
	latencyTool
	latencyHeartbeat
	latencyEventCount
)

type requestLatencyKey struct{}
type requestLatency struct {
	started    time.Time
	prompt     atomic.Pointer[requestPromptMetadata]
	attachment atomic.Pointer[bool]
	events     [latencyEventCount]atomic.Int64 // -1 absent; otherwise elapsed nanoseconds
}

type requestPromptMetadata struct{ formattedBytes, budgetBytes int64 }

type requestLatencySnapshot struct {
	PromptBytes, PromptBudgetBytes         *int64
	ContextAttachmentUsed                  *bool
	PrepareStart, PrepareEnd               *time.Duration
	Started                                time.Time
	Elapsed                                time.Duration
	CatalogStart, CatalogEnd, Submitted    *time.Duration
	UpstreamText, Reasoning, ModelVerified *time.Duration
	DownstreamText, Tool, Heartbeat        *time.Duration
}

// Attach once at request ingress, before catalog lookups or other preparation.
// Children share the same monotonic origin. Rewrapping must not reset it.
func withRequestLatency(parent context.Context) context.Context {
	if _, ok := parent.Value(requestLatencyKey{}).(*requestLatency); ok {
		return parent
	}
	r := &requestLatency{started: time.Now()}
	for i := range r.events {
		r.events[i].Store(-1)
	}
	return context.WithValue(parent, requestLatencyKey{}, r)
}

// Marking is allocation-free and lock-free. It is a no-op without an attached
// tracker, for invalid events, or after this event has already been observed.
// The first successful CAS wins if multiple goroutines race to mark an event.
func markRequestLatency(ctx context.Context, event latencyEvent) {
	r, _ := ctx.Value(requestLatencyKey{}).(*requestLatency)
	if r == nil || event >= latencyEventCount || r.events[event].Load() >= 0 {
		return
	}
	elapsed := time.Since(r.started)
	if elapsed < 0 {
		elapsed = 0
	}
	r.events[event].CompareAndSwap(-1, int64(elapsed))
}

// Snapshot reads events independently: it is safe during generation, but not a
// transaction across events. nil is missing, including on requests without a
// tracker; a pointer to zero is a genuine observed zero-duration event.
func snapshotRequestLatency(ctx context.Context) requestLatencySnapshot {
	r, _ := ctx.Value(requestLatencyKey{}).(*requestLatency)
	if r == nil {
		return requestLatencySnapshot{}
	}
	value := func(event latencyEvent) *time.Duration {
		n := r.events[event].Load()
		if n < 0 {
			return nil
		}
		d := time.Duration(n)
		return &d
	}
	out := requestLatencySnapshot{
		PrepareStart: value(latencyPrepareStart), PrepareEnd: value(latencyPrepareEnd),
		Started: r.started, Elapsed: time.Since(r.started),
		CatalogStart: value(latencyCatalogStart), CatalogEnd: value(latencyCatalogEnd), Submitted: value(latencySubmitted),
		UpstreamText: value(latencyUpstreamText), Reasoning: value(latencyReasoning), ModelVerified: value(latencyModelVerified),
		DownstreamText: value(latencyDownstreamText), Tool: value(latencyTool), Heartbeat: value(latencyHeartbeat),
	}
	if p := r.prompt.Load(); p != nil {
		size, budget := p.formattedBytes, p.budgetBytes
		out.PromptBytes, out.PromptBudgetBytes = &size, &budget
	}
	if used := r.attachment.Load(); used != nil {
		value := *used
		out.ContextAttachmentUsed = &value
	}
	return out

}

// Metadata records counts only, never prompt contents. The first valid note wins
// and the byte count/budget pair is published atomically as one observation.
func noteRequestPrompt(ctx context.Context, formattedBytes, budgetBytes int) {
	r, _ := ctx.Value(requestLatencyKey{}).(*requestLatency)
	if r == nil || formattedBytes < 0 || budgetBytes < 0 || r.prompt.Load() != nil {
		return
	}
	r.prompt.CompareAndSwap(nil, &requestPromptMetadata{int64(formattedBytes), int64(budgetBytes)})
}
func noteContextAttachment(ctx context.Context, used bool) {
	r, _ := ctx.Value(requestLatencyKey{}).(*requestLatency)
	if r == nil || r.attachment.Load() != nil {
		return
	}
	r.attachment.CompareAndSwap(nil, &used)
}
