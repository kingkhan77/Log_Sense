package worker

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shahzad/logsense/internal/ai"
	"github.com/shahzad/logsense/internal/db"
	"github.com/shahzad/logsense/internal/models"
)

const (
	ErrorRateThreshold         = 0.05
	LatencyMultiplierThreshold = 2.0
	BaselineWindowCount        = 10
	DrainBatchSize             = 500
)

// Worker holds the dependencies needed for background processing.
//
// AI is typed as ai.Summarizer — the interface — not *ai.OpenAIClient.
// This means the worker has no knowledge of which provider is behind it.
// Swap OpenAI for Gemini in main.go and this file doesn't change at all.
// nil = summarization disabled (service runs in degraded mode).
type Worker struct {
	PG       *db.PGPool
	Redis    *db.RedisClient
	AI       ai.Summarizer // interface, not concrete type
	interval time.Duration
}

func New(pg *db.PGPool, redis *db.RedisClient, aiClient ai.Summarizer, interval time.Duration) *Worker {
	return &Worker{PG: pg, Redis: redis, AI: aiClient, interval: interval}
}

// Start runs the processing loop until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	log.Printf("[worker] starting — tick interval: %s", w.interval)

	for {
		select {
		case <-ticker.C:
			w.runCycle(ctx)
		case <-ctx.Done():
			log.Println("[worker] context cancelled — shutting down")
			return
		}
	}
}

// runCycle: drain Redis → persist log_events → aggregate metrics → detect anomalies.
func (w *Worker) runCycle(ctx context.Context) {
	cycleStart := time.Now()

	rawEvents, err := w.Redis.DrainEvents(ctx, DrainBatchSize)
	if err != nil {
		log.Printf("[worker] drain error: %v", err)
		return
	}
	if len(rawEvents) == 0 {
		log.Println("[worker] no events to process")
		return
	}
	log.Printf("[worker] drained %d events from queue", len(rawEvents))

	events := make([]models.LogEvent, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var e models.LogEvent
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			log.Printf("[worker] skipping malformed event: %v", err)
			continue
		}
		events = append(events, e)
	}

	if err := w.PG.BulkInsertLogEvents(ctx, events); err != nil {
		log.Printf("[worker] bulk insert error: %v", err)
		// Don't return — aggregate from in-memory slice rather than losing the cycle
	}

	byService := groupByService(events)
	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-w.interval)

	for serviceName, svcEvents := range byService {
		metric := aggregate(serviceName, svcEvents, windowStart, windowEnd)

		if err := w.PG.InsertLogMetric(ctx, metric); err != nil {
			log.Printf("[worker] insert metric error for %s: %v", serviceName, err)
			continue
		}

		w.detectAnomalies(ctx, metric)
	}

	log.Printf("[worker] cycle complete in %s — %d events, %d services",
		time.Since(cycleStart).Round(time.Millisecond),
		len(events),
		len(byService),
	)
}

func groupByService(events []models.LogEvent) map[string][]models.LogEvent {
	groups := make(map[string][]models.LogEvent)
	for _, e := range events {
		groups[e.ServiceName] = append(groups[e.ServiceName], e)
	}
	return groups
}

func aggregate(serviceName string, events []models.LogEvent, windowStart, windowEnd time.Time) models.LogMetric {
	total := len(events)
	errorCount := 0
	latencies := make([]float64, 0, total)

	for _, e := range events {
		latencies = append(latencies, e.LatencyMs)
		if e.Level == models.LogLevelError || e.StatusCode >= 500 {
			errorCount++
		}
	}

	sort.Float64s(latencies)

	return models.LogMetric{
		ID:           uuid.New().String(),
		ServiceName:  serviceName,
		WindowStart:  windowStart,
		WindowEnd:    windowEnd,
		P95LatencyMs: computeP95(latencies),
		ErrorRate:    float64(errorCount) / float64(total),
		TotalEvents:  total,
	}
}

func computeP95(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func (w *Worker) detectAnomalies(ctx context.Context, metric models.LogMetric) {
	if metric.ErrorRate > ErrorRateThreshold {
		w.maybeCreateIncident(ctx, metric, "high_error_rate", map[string]any{
			"error_rate":     metric.ErrorRate,
			"threshold":      ErrorRateThreshold,
			"total_events":   metric.TotalEvents,
			"window_start":   metric.WindowStart,
			"window_end":     metric.WindowEnd,
			"p95_latency_ms": metric.P95LatencyMs,
		})
	}

	if metric.P95LatencyMs > 0 {
		baseline, err := w.PG.GetRecentMetricBaseline(ctx, metric.ServiceName, BaselineWindowCount)
		if err != nil {
			log.Printf("[worker] baseline fetch error for %s: %v", metric.ServiceName, err)
		} else if baseline > 0 && metric.P95LatencyMs > LatencyMultiplierThreshold*baseline {
			w.maybeCreateIncident(ctx, metric, "high_latency", map[string]any{
				"p95_latency_ms":  metric.P95LatencyMs,
				"baseline_p95_ms": baseline,
				"multiplier":      metric.P95LatencyMs / baseline,
				"threshold":       LatencyMultiplierThreshold,
				"total_events":    metric.TotalEvents,
				"window_start":    metric.WindowStart,
				"window_end":      metric.WindowEnd,
			})
		}
	}
}

// maybeCreateIncident gates on deduplication, creates the incident row,
// then asynchronously calls OpenAI to generate and store the ai_summary.
//
// The AI call runs in a separate goroutine so it never blocks the worker cycle.
// If OpenAI is slow or down, the incident is still created immediately —
// the summary just arrives a few seconds later (or not at all if it fails).
// This is the "critical path vs enrichment" pattern: don't let optional
// work block required work.
func (w *Worker) maybeCreateIncident(ctx context.Context, metric models.LogMetric, incidentType string, rawContext map[string]any) {
	exists, err := w.PG.GetOpenIncidentExists(ctx, metric.ServiceName, incidentType)
	if err != nil {
		log.Printf("[worker] dedup check error: %v", err)
		return
	}
	if exists {
		log.Printf("[worker] open %s incident already exists for %s — skipping",
			incidentType, metric.ServiceName)
		return
	}

	incident := models.Incident{
		ID:          uuid.New().String(),
		ServiceName: metric.ServiceName,
		Type:        incidentType,
		Status:      models.IncidentStatusOpen,
		RawContext:  rawContext,
		AISummary:   "",
		CreatedAt:   time.Now().UTC(),
	}

	if err := w.PG.InsertIncident(ctx, incident); err != nil {
		log.Printf("[worker] create incident error: %v", err)
		return
	}

	log.Printf("[worker] new incident created — service=%s type=%s id=%s",
		metric.ServiceName, incidentType, incident.ID)

	// Kick off AI summarization in a goroutine — non-blocking.
	// We pass a fresh background context (not the worker's ctx) because
	// the worker cycle context may be cancelled before OpenAI responds.
	// We want the summary to complete even if the next tick starts.
	if w.AI != nil {
		go w.summarizeIncident(context.Background(), incident)
	}
}

// summarizeIncident calls OpenAI and updates the incident row with the result.
// Runs in its own goroutine — all errors are logged, never propagated.
func (w *Worker) summarizeIncident(ctx context.Context, incident models.Incident) {
	log.Printf("[worker] requesting AI summary for incident %s (provider: %s)",
		incident.ID, w.AI.Provider())

	// 45s timeout for the OpenAI call — generous but bounded.
	// The HTTP client has its own 30s timeout, this is an outer safety net.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	summary, err := w.AI.SummarizeIncident(ctx, incident.ServiceName, incident.Type, incident.RawContext)
	if err != nil {
		log.Printf("[worker] AI summarization failed for incident %s: %v", incident.ID, err)
		return
	}

	if err := w.PG.UpdateIncidentSummary(ctx, incident.ID, summary); err != nil {
		log.Printf("[worker] failed to store AI summary for incident %s: %v", incident.ID, err)
		return
	}

	log.Printf("[worker] AI summary stored for incident %s", incident.ID)
}
