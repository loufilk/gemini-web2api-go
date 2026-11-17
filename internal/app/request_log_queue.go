package app

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const requestLogCapacity = 2048

// Request logs are best-effort diagnostic metadata, not a billing ledger.
// Keep a bounded queue off the response path and never leave unbounded writer
// goroutines behind a stalled database.
type requestLogWorker struct {
	mu      sync.Mutex
	closed  bool
	queue   chan *RequestRow
	done    chan struct{}
	cancel  context.CancelFunc
	dropped atomic.Uint64
}

var requestLogQueue = sync.OnceValue(func() *requestLogWorker {
	return newRequestLogWorker(requestLogCapacity, insertRequestContext)
})

func newRequestLogWorker(capacity int, write func(context.Context, *RequestRow)) *requestLogWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &requestLogWorker{queue: make(chan *RequestRow, capacity), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(w.done)
		for row := range w.queue {
			if ctx.Err() != nil {
				return
			}
			writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			write(writeCtx, row)
			stop()
		}
	}()
	return w
}

func (w *requestLogWorker) enqueue(row *RequestRow) bool {
	w.mu.Lock()
	if !w.closed {
		select {
		case w.queue <- row:
			w.mu.Unlock()
			return true
		default:
		}
	}
	w.mu.Unlock()
	// Powers of two give an operator feedback without flooding stderr at load.
	if n := w.dropped.Add(1); n&(n-1) == 0 {
		fmt.Fprintf(os.Stderr, "[request-log] queue unavailable; dropped metadata rows: %d\n", n)
	}
	return false
}

func (w *requestLogWorker) stop(ctx context.Context) bool {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	defer w.cancel()
	select {
	case <-w.done:
		return true
	case <-ctx.Done():
		return false
	}
}
