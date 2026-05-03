package models

import (
	"time"
)

// LogLevel represents the severity of a log event.
// Using a string type alias makes the intent clear and prevents
// accidental assignment of arbitrary strings in business logic.
type LogLevel string

const (
	LogLevelInfo  LogLevel = "INFO"
	LogLevelWarn  LogLevel = "WARN"
	LogLevelError LogLevel = "ERROR"
)

// IncidentStatus represents the lifecycle state of an incident.
type IncidentStatus string

const (
	IncidentStatusOpen     IncidentStatus = "open"
	IncidentStatusResolved IncidentStatus = "resolved"
)

// LogEvent is the raw ingested event pushed into Redis and eventually
// persisted to log_events. This is the primary unit of data in the pipeline.
type LogEvent struct {
	ID          string    `json:"id"`
	ServiceName string    `json:"service_name"`
	Level       LogLevel  `json:"level"`
	Message     string    `json:"message"`
	LatencyMs   float64   `json:"latency_ms"`  // request latency in milliseconds
	StatusCode  int       `json:"status_code"`
	Timestamp   time.Time `json:"timestamp"`
}

// LogMetric is an aggregated snapshot produced by the worker every 30s.
// It captures p95 latency and error rate for a service within a time window.
type LogMetric struct {
	ID           string    `json:"id"`
	ServiceName  string    `json:"service_name"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	P95LatencyMs float64   `json:"p95_latency_ms"`
	ErrorRate    float64   `json:"error_rate"`    // 0.0 to 1.0 (e.g., 0.07 = 7%)
	TotalEvents  int       `json:"total_events"`
	CreatedAt    time.Time `json:"created_at"`
}

// Incident is created when the worker detects an anomaly.
// RawContext is a JSON blob — we store whatever data the worker had at the
// time of detection, without committing to a rigid schema. This is the key
// advantage of JSONB in PostgreSQL.
type Incident struct {
	ID         string         `json:"id"`
	ServiceName string        `json:"service_name"`
	Type       string         `json:"type"`        // e.g., "high_error_rate", "high_latency"
	Status     IncidentStatus `json:"status"`
	RawContext map[string]any `json:"raw_context"` // JSONB — stores metric snapshot
	AISummary  string         `json:"ai_summary"`  // populated by OpenAI call
	CreatedAt  time.Time      `json:"created_at"`
	ResolvedAt *time.Time     `json:"resolved_at"` // pointer so it can be null
}

// IngestRequest is the expected shape of POST /ingest body.
// Separating this from LogEvent lets us evolve the API independently.
type IngestRequest struct {
	ServiceName string   `json:"service_name" binding:"required"`
	Level       LogLevel `json:"level"        binding:"required"`
	Message     string   `json:"message"      binding:"required"`
	LatencyMs   float64  `json:"latency_ms"`
	StatusCode  int      `json:"status_code"`
}