package worker

import (
	"context"
	"log"
	"time"

	"github.com/shahzad/logsense/internal/db"
)

// Worker holds the dependencies needed for background processing.
// Same pattern as Handler — explicit dependency injection, no globals.
type Worker struct {
	PG       *db.PGPool
	Redis    *db.RedisClient
	interval time.Duration
}

// New creates a Worker with a configurable tick interval.
// Accepting `interval` as a parameter (rather than hardcoding 30s) makes
// testing faster — you can pass 100ms in tests without waiting 30s per cycle.
func New(pg *db.PGPool, redis *db.RedisClient, interval time.Duration) *Worker {
	return &Worker{
		PG:       pg,
		Redis:    redis,
		interval: interval,
	}
}

// Start launches the background processing loop.
//
// The pattern here is important: we use a select on two channels —
//   - ticker.C fires every `interval` to trigger a processing cycle
//   - ctx.Done() fires when the caller cancels (e.g., on SIGTERM shutdown)
//
// This is the idiomatic Go way to build a cancellable background goroutine.
// Without the ctx.Done() case, the goroutine would keep running even after
// main() tries to shut down, causing a goroutine leak.
//
// Call this in a goroutine: go worker.Start(ctx)
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop() // always stop the ticker to free resources

	log.Printf("[worker] starting — tick interval: %s", w.interval)

	for {
		select {
		case <-ticker.C:
			// Each tick runs in the same goroutine as Start.
			// This means if a cycle takes longer than `interval`, the next
			// tick will be delayed — not run concurrently. That's intentional:
			// we don't want overlapping cycles reading the same Redis data.
			w.runCycle(ctx)

		case <-ctx.Done():
			log.Println("[worker] context cancelled — shutting down")
			return
		}
	}
}

// runCycle is one full processing pass.
// Day 1: just a stub that logs "tick". Days 2-4 will fill this in.
func (w *Worker) runCycle(ctx context.Context) {
	log.Println("[worker] tick — drain → aggregate → detect → summarize (stub)")
	// Day 2: drain Redis queue, persist log_events, aggregate metrics
	// Day 3: anomaly detection → create incidents
	// Day 4: OpenAI summarization on new incidents
}