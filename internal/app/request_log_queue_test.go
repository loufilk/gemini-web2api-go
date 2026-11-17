package app

import (
	"context"
	"testing"
	"time"
)

func TestRequestLogQueueBoundsAndDrains(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	written := make(chan int, 2)
	w := newRequestLogWorker(1, func(_ context.Context, row *RequestRow) {
		if row.Status == 1 {
			close(started)
			<-release
		}
		written <- row.Status
	})
	if !w.enqueue(&RequestRow{Status: 1}) {
		t.Fatal("first write dropped")
	}
	<-started
	if !w.enqueue(&RequestRow{Status: 2}) || w.enqueue(&RequestRow{Status: 3}) {
		t.Fatal("queue failed to cap backlog")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !w.stop(ctx) {
		t.Fatal("failed to drain")
	}
	if got1, got2 := <-written, <-written; got1 != 1 || got2 != 2 {
		t.Fatalf("drain wrote %d then %d", got1, got2)
	}
	if w.enqueue(&RequestRow{Status: 4}) || w.dropped.Load() != 2 {
		t.Fatal("stopped queue accepted work or did not count drops")
	}
}

func TestRequestLogQueueCancellationStopsStalledWrite(t *testing.T) {
	started := make(chan struct{})
	observed := make(chan struct{})
	w := newRequestLogWorker(1, func(ctx context.Context, _ *RequestRow) {
		close(started)
		<-ctx.Done()
		close(observed)
	})
	w.enqueue(&RequestRow{Status: 200})
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w.stop(ctx) {
		t.Fatal("stalled drain reported complete")
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("stalled database write not canceled")
	}
}
