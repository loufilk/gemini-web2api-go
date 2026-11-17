package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Optional phase limits are independent of downstream SSE keepalives. A zero
// first-frame/idle limit leaves that phase unrestricted by this helper.
type generationTimeouts struct{ Overall, FirstFrame, Idle time.Duration }
type generationTimeoutsKey struct{}

func withGenerationTimeouts(ctx context.Context, limits generationTimeouts) context.Context {
	return context.WithValue(ctx, generationTimeoutsKey{}, limits)
}

type generationTiming struct {
	FirstFrame, MaxIdle, Total time.Duration
	Frames                     int
}
type generationObserverKey struct{}

// The observer is called synchronously once at completion, with no payload or credentials.
func withGenerationObserver(ctx context.Context, observe func(generationTiming)) context.Context {
	return context.WithValue(ctx, generationObserverKey{}, observe)
}

type generationPhaseTimeout struct{ Phase string }

func (e *generationPhaseTimeout) Error() string { return fmt.Sprintf("generation %s timeout", e.Phase) }
func (e *generationPhaseTimeout) Is(target error) bool {
	return target == context.DeadlineExceeded || target == errUpstreamTimeout
}

type generationControl struct {
	ctx            context.Context
	cancel         context.CancelCauseFunc
	parent         context.Context
	mu             sync.Mutex
	started, last  time.Time
	timing         generationTiming
	overall, phase *time.Timer
	limits         generationTimeouts
	finished       bool
}

func startGenerationContext(parent context.Context, fallback time.Duration) *generationControl {
	limits, _ := parent.Value(generationTimeoutsKey{}).(generationTimeouts)
	if limits.Overall <= 0 {
		limits.Overall = fallback
	}
	ctx, cancel := context.WithCancelCause(parent)
	g := &generationControl{ctx: ctx, cancel: cancel, parent: parent, started: time.Now(), limits: limits}
	if limits.Overall > 0 {
		g.overall = time.AfterFunc(limits.Overall, func() { cancel(&generationPhaseTimeout{Phase: "overall"}) })
	}
	if limits.FirstFrame > 0 {
		g.phase = time.AfterFunc(limits.FirstFrame, func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if !g.finished && g.timing.Frames == 0 {
				cancel(&generationPhaseTimeout{Phase: "first_frame"})
			}
		})
	}
	return g
}
func (g *generationControl) frame() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.finished {
		return
	}
	now := time.Now()
	if g.timing.Frames == 0 {
		g.timing.FirstFrame = now.Sub(g.started)
	} else if idle := now.Sub(g.last); idle > g.timing.MaxIdle {
		g.timing.MaxIdle = idle
	}
	g.timing.Frames++
	g.last = now
	if g.phase != nil {
		g.phase.Stop()
	}
	if g.limits.Idle > 0 {
		last := now
		g.phase = time.AfterFunc(g.limits.Idle, func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if !g.finished && g.last.Equal(last) {
				g.cancel(&generationPhaseTimeout{Phase: "idle"})
			}
		})
	}
}
func (g *generationControl) err(err error) error {
	// Caller cancellation/deadline must not be mislabeled an upstream timeout.
	if g.parent.Err() != nil {
		return g.parent.Err()
	}
	if cause := context.Cause(g.ctx); cause != nil {
		return cause
	}
	return err
}
func (g *generationControl) finish() {
	g.mu.Lock()
	if g.finished {
		g.mu.Unlock()
		return
	}
	g.finished = true
	if g.overall != nil {
		g.overall.Stop()
	}
	if g.phase != nil {
		g.phase.Stop()
	}
	g.timing.Total = time.Since(g.started)
	if !g.last.IsZero() {
		if idle := time.Since(g.last); idle > g.timing.MaxIdle {
			g.timing.MaxIdle = idle
		}
	}
	timing := g.timing
	g.mu.Unlock()
	g.cancel(context.Canceled)
	if observe, ok := g.parent.Value(generationObserverKey{}).(func(generationTiming)); ok && observe != nil {
		observe(timing)
	}
}

func generationLineHasContent(line string) bool {
	return len(textsInLine(line)) > 0 || reasoningInLine(line) != ""
}

// Cancel the caller-owned branch if a streaming write fails before net/http
// otherwise reports the disconnect. Never cancel on an unsupported flush.
type generationResponseWriter struct {
	http.ResponseWriter
	cancel context.CancelFunc
}

func (w generationResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w generationResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.cancel()
	}
	return n, err
}
func (w generationResponseWriter) Flush() {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.cancel()
	}
}
func bindGenerationWriter(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	return generationResponseWriter{w, cancel}, r.WithContext(ctx), cancel
}

var errUpstreamIncomplete = errors.New("upstream ended without completion")
