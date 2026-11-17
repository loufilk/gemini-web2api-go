package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func awaitGeneration(t *testing.T, g *generationControl) error {
	t.Helper()
	select {
	case <-g.ctx.Done():
		return g.err(g.ctx.Err())
	case <-time.After(time.Second):
		t.Fatal("context did not stop")
		return nil
	}
}
func TestGenerationCallerCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	g := startGenerationContext(parent, time.Second)
	defer g.finish()
	cancel()
	err := awaitGeneration(t, g)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errUpstreamTimeout) {
		t.Fatalf("caller cancellation misclassified: %v", err)
	}
}
func TestGenerationCallerDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	g := startGenerationContext(parent, time.Second)
	defer g.finish()
	err := awaitGeneration(t, g)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errUpstreamTimeout) {
		t.Fatalf("caller deadline misclassified: %v", err)
	}
}
func TestGenerationPhaseDeadlines(t *testing.T) {
	for _, phase := range []string{"overall", "first_frame", "idle"} {
		t.Run(phase, func(t *testing.T) {
			limits := generationTimeouts{}
			switch phase {
			case "overall":
				limits.Overall = time.Millisecond
			case "first_frame":
				limits.FirstFrame = time.Millisecond
			case "idle":
				limits.Idle = time.Millisecond
			}
			g := startGenerationContext(withGenerationTimeouts(context.Background(), limits), time.Second)
			defer g.finish()
			if phase == "idle" {
				g.frame()
			}
			err := awaitGeneration(t, g)
			var timeout *generationPhaseTimeout
			if !errors.As(err, &timeout) || timeout.Phase != phase || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errUpstreamTimeout) {
				t.Fatalf("wrong timeout: %v", err)
			}
		})
	}
}
func TestGenerationFirstFrameTransitionsToIdle(t *testing.T) {
	ctx := withGenerationTimeouts(context.Background(), generationTimeouts{FirstFrame: 20 * time.Millisecond, Idle: time.Second})
	g := startGenerationContext(ctx, time.Second)
	defer g.finish()
	g.frame()
	select {
	case <-g.ctx.Done():
		t.Fatal("first frame timer was not stopped")
	case <-time.After(30 * time.Millisecond):
	}
}
func TestGenerationObservation(t *testing.T) {
	calls := 0
	var got generationTiming
	ctx := withGenerationObserver(context.Background(), func(m generationTiming) { calls++; got = m })
	g := startGenerationContext(ctx, time.Second)
	g.frame()
	g.frame()
	g.finish()
	if calls != 1 || got.Frames != 2 || got.Total <= 0 || got.FirstFrame < 0 || got.MaxIdle < 0 {
		t.Fatalf("unexpected timing %+v calls=%d", got, calls)
	}
}

// Pre-canceled preparation must stop before selecting a transport or reading cache.
func TestGenerationPreparationCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := getXSRFContext(ctx, "", 0, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("getXSRF: %v", err)
	}
	if _, err := fetchAppPageContext(ctx, "", 0, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchAppPage: %v", err)
	}
	if _, err := webChannelAPIKeyContext(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("webChannelAPIKey: %v", err)
	}
}
