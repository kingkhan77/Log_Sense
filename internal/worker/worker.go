package worker

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shahzad/logsense/internal/db"
	"github.com/shahzad/logsense/internal/models"
)

const (
	ErrorRateThreshold         = 0.05
	LatencyMultiplierThreshold = 2.0
	BaselineWindowCount        = 10
	DrainBatchSize             = 500
)

type Worker struct {
	PG       *db.PGPool
	Redis    *db.RedisClient
	interval time.Duration
}

func New(pg *db.PGPool, redis *db.RedisClient, interval time.Duration) *Worker {
	return &Worker{
		PG:       pg,
		Redis:    redis,
		interval: interval,
	}
}

func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.runCycle(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (w *Worker) runCycle(ctx context.Context) {
	rawEvents, err := w.Redis.DrainEvents(ctx, DrainBatchSize)
	if err != nil {
		log.Printf("[worker] drain error: %v", err)
		return
	}

	if len(rawEvents) == 0 {
		log.Println("[worker] no events to process")
		return
	}

	events := make([]models.LogEvent, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var event models.LogEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			log.Printf("[worker] malformed event skipped: %v", err)
			continue
		}
		events = append(events, event)
	}

	if err := w.PG.BulkInsertLogEvents(ctx, events); err != nil {
		log.Printf("[worker] bulk insert log_events error: %v", err)
	}

	windowEnd := time.Now().UTC()
	windowStart := windowEnd.Add(-w.interval)
	eventsByService := groupByService(events)

	for serviceName, serviceEvents := range eventsByService {
		metric := aggregate(serviceName, serviceEvents, windowStart, windowEnd)
		if err := w.PG.InsertLogMetric(ctx, metric); err != nil {
			log.Printf("[worker] insert metric failed for service=%s: %v", serviceName, err)
			continue
		}

		w.detectAnomalies(ctx, metric)
	}
}

func groupByService(events []models.LogEvent) map[string][]models.LogEvent {
	grouped := make(map[string][]models.LogEvent)
	for _, event := range events {
		grouped[event.ServiceName] = append(grouped[event.ServiceName], event)
	}
	return grouped
}

func aggregate(serviceName string, events []models.LogEvent, windowStart, windowEnd time.Time) models.LogMetric {
	total := len(events)
	errorCount := 0
	latencies := make([]float64, 0, total)

	for _, event := range events {
		latencies = append(latencies, event.LatencyMs)
		if event.Level == models.LogLevelError || event.StatusCode >= 500 {
			errorCount++
		}
	}

	sort.Float64s(latencies)

	p95 := 0.0
	if total > 0 {
		index := int(math.Ceil(0.95*float64(total))) - 1
		if index < 0 {
			index = 0
		}
		if index >= total {
			index = total - 1
		}
		p95 = latencies[index]
	}

	errorRate := 0.0
	if total > 0 {
		errorRate = float64(errorCount) / float64(total)
	}

	return models.LogMetric{
		ID:           uuid.NewString(),
		ServiceName:  serviceName,
		WindowStart:  windowStart,
		WindowEnd:    windowEnd,
		P95LatencyMs: p95,
		ErrorRate:    errorRate,
		TotalEvents:  total,
		CreatedAt:    time.Now().UTC(),
	}
}

func (w *Worker) detectAnomalies(ctx context.Context, metric models.LogMetric) {
	if metric.ErrorRate > ErrorRateThreshold {
		w.maybeCreateIncident(ctx, metric, "high_error_rate", map[string]any{
			"error_rate":      metric.ErrorRate,
			"threshold":       ErrorRateThreshold,
			"service_name":    metric.ServiceName,
			"window_start":    metric.WindowStart,
			"window_end":      metric.WindowEnd,
			"p95_latency_ms":  metric.P95LatencyMs,
			"total_events":    metric.TotalEvents,
			"incident_reason": "error_rate_threshold_breach",
		})
	}

	baseline, err := w.PG.GetRecentMetricBaseline(ctx, metric.ServiceName, BaselineWindowCount)
	if err != nil {
		log.Printf("[worker] baseline lookup failed for service=%s: %v", metric.ServiceName, err)
		return
	}

	if baseline > 0 && metric.P95LatencyMs > LatencyMultiplierThreshold*baseline {
		w.maybeCreateIncident(ctx, metric, "high_latency", map[string]any{
			"service_name":    metric.ServiceName,
			"p95_latency_ms":  metric.P95LatencyMs,
			"baseline_p95_ms": baseline,
			"multiplier":      metric.P95LatencyMs / baseline,
			"threshold":       LatencyMultiplierThreshold,
			"window_start":    metric.WindowStart,
			"window_end":      metric.WindowEnd,
			"total_events":    metric.TotalEvents,
			"incident_reason": "latency_multiplier_breach",
		})
	}
}

func (w *Worker) maybeCreateIncident(
	ctx context.Context,
	metric models.LogMetric,
	incidentType string,
	rawContext map[string]any,
) {
	exists, err := w.PG.GetOpenIncidentExists(ctx, metric.ServiceName, incidentType)
	if err != nil {
		log.Printf("[worker] failed dedup check for service=%s type=%s: %v", metric.ServiceName, incidentType, err)
		return
	}

	if exists {
		log.Printf("[worker] open incident already exists for service=%s type=%s", metric.ServiceName, incidentType)
		return
	}

	incident := models.Incident{
		ID:          uuid.NewString(),
		ServiceName: metric.ServiceName,
		Type:        incidentType,
		Status:      models.IncidentStatusOpen,
		RawContext:  rawContext,
		AISummary:   "",
		CreatedAt:   time.Now().UTC(),
	}

	if err := w.PG.InsertIncident(ctx, incident); err != nil {
		log.Printf("[worker] failed to insert incident for service=%s type=%s: %v", metric.ServiceName, incidentType, err)
		return
	}
}