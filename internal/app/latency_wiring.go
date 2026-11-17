package app

import (
	"context"
	"net/http"
	"time"
)

func latencyMillis(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := d.Milliseconds()
	return &ms
}

func latencyBetweenMillis(start, end *time.Duration) *int64 {
	if start == nil || end == nil || *end < *start {
		return nil
	}
	elapsed := *end - *start
	return latencyMillis(&elapsed)
}

func markLatencyAfterWrite(ctx context.Context, event latencyEvent) {
	if ctx.Err() == nil {
		markRequestLatency(ctx, event)
	}
}

func measureTextOutput(ctx context.Context, emit func(string)) func(string) {
	return func(text string) {
		if text == "" || ctx.Err() != nil {
			return
		}
		emit(text)
		markLatencyAfterWrite(ctx, latencyDownstreamText)
	}
}

func completedWriteStatus(ctx context.Context) (int, string) {
	if ctx.Err() != nil {
		return generationFailure(ctx.Err())
	}
	return http.StatusOK, ""
}

func noteGenerationContent(ctx context.Context, line string) bool {
	hasText := len(textsInLine(line)) > 0
	if hasText {
		markRequestLatency(ctx, latencyUpstreamText)
	}
	if reasoningInLine(line) != "" {
		markRequestLatency(ctx, latencyReasoning)
		return true
	}
	return hasText
}
