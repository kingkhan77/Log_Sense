package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shahzad/logsense/internal/models"
)

func (p *PGPool) BulkInsertLogEvents(ctx context.Context, events []models.LogEvent) error {
	if len(events) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString(`
INSERT INTO log_events (id, service_name, level, message, latency_ms, status_code, timestamp)
VALUES `)

	args := make([]any, 0, len(events)*7)
	for i, e := range events {
		if i > 0 {
			sb.WriteString(",")
		}

		base := i*7 + 1
		sb.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4, base+5, base+6))
		args = append(args, e.ID, e.ServiceName, e.Level, e.Message, e.LatencyMs, e.StatusCode, e.Timestamp)
	}

	sb.WriteString(" ON CONFLICT (id) DO NOTHING")

	_, err := p.Pool.Exec(ctx, sb.String(), args...)
	if err != nil {
		return fmt.Errorf("bulk insert log_events failed: %w", err)
	}
	return nil
}

func (p *PGPool) InsertLogMetric(ctx context.Context, m models.LogMetric) error {
	query := `
INSERT INTO log_metrics (id, service_name, window_start, window_end, p95_latency_ms, error_rate, total_events, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`
	_, err := p.Pool.Exec(
		ctx,
		query,
		m.ID,
		m.ServiceName,
		m.WindowStart,
		m.WindowEnd,
		m.P95LatencyMs,
		m.ErrorRate,
		m.TotalEvents,
		m.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert log_metric failed: %w", err)
	}
	return nil
}

func (p *PGPool) GetRecentMetricBaseline(ctx context.Context, serviceName string, windowCount int) (float64, error) {
	query := `
SELECT COALESCE(AVG(p95_latency_ms), 0)
FROM (
	SELECT p95_latency_ms
	FROM log_metrics
	WHERE service_name = $1
	ORDER BY window_end DESC
	LIMIT $2
) AS recent
`
	var baseline float64
	if err := p.Pool.QueryRow(ctx, query, serviceName, windowCount).Scan(&baseline); err != nil {
		return 0, fmt.Errorf("get recent metric baseline failed: %w", err)
	}
	return baseline, nil
}

func (p *PGPool) GetRecentMetrics(ctx context.Context, limit int) ([]models.LogMetric, error) {
	query := `
SELECT id, service_name, window_start, window_end, p95_latency_ms, error_rate, total_events, created_at
FROM log_metrics
ORDER BY window_end DESC
LIMIT $1
`
	rows, err := p.Pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent metrics failed: %w", err)
	}
	defer rows.Close()

	metrics := make([]models.LogMetric, 0, limit)
	for rows.Next() {
		var m models.LogMetric
		if err := rows.Scan(
			&m.ID,
			&m.ServiceName,
			&m.WindowStart,
			&m.WindowEnd,
			&m.P95LatencyMs,
			&m.ErrorRate,
			&m.TotalEvents,
			&m.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recent metric failed: %w", err)
		}
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent metrics failed: %w", err)
	}
	return metrics, nil
}

func (p *PGPool) GetOpenIncidentExists(ctx context.Context, serviceName, incidentType string) (bool, error) {
	query := `
SELECT EXISTS (
	SELECT 1
	FROM incidents
	WHERE service_name = $1
	  AND type = $2
	  AND status = 'open'
)
`
	var exists bool
	if err := p.Pool.QueryRow(ctx, query, serviceName, incidentType).Scan(&exists); err != nil {
		return false, fmt.Errorf("check open incident exists failed: %w", err)
	}
	return exists, nil
}

func (p *PGPool) InsertIncident(ctx context.Context, i models.Incident) error {
	rawContext, err := json.Marshal(i.RawContext)
	if err != nil {
		return fmt.Errorf("marshal incident raw_context failed: %w", err)
	}

	query := `
INSERT INTO incidents (id, service_name, type, status, raw_context, ai_summary, created_at, resolved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`
	_, err = p.Pool.Exec(
		ctx,
		query,
		i.ID,
		i.ServiceName,
		i.Type,
		i.Status,
		rawContext,
		i.AISummary,
		i.CreatedAt,
		i.ResolvedAt,
	)
	if err != nil {
		return fmt.Errorf("insert incident failed: %w", err)
	}
	return nil
}

func (p *PGPool) UpdateIncidentSummary(ctx context.Context, incidentID, summary string) error {
	query := `
UPDATE incidents
SET ai_summary = $2
WHERE id = $1
`
	cmdTag, err := p.Pool.Exec(ctx, query, incidentID, summary)
	if err != nil {
		return fmt.Errorf("update incident summary failed: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return fmt.Errorf("incident not found: %s", incidentID)
	}
	return nil
}

func (p *PGPool) ResolveIncident(ctx context.Context, incidentID string) error {
	query := `
UPDATE incidents
SET status = 'resolved', resolved_at = NOW()
WHERE id = $1
  AND status = 'open'
`
	cmdTag, err := p.Pool.Exec(ctx, query, incidentID)
	if err != nil {
		return fmt.Errorf("resolve incident failed: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return errors.New("incident not found or already resolved")
	}
	return nil
}

func (p *PGPool) GetIncidents(ctx context.Context) ([]models.Incident, error) {
	query := `
SELECT id, service_name, type, status, raw_context, ai_summary, created_at, resolved_at
FROM incidents
ORDER BY created_at DESC
LIMIT 100
`
	rows, err := p.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query incidents failed: %w", err)
	}
	defer rows.Close()

	incidents := make([]models.Incident, 0, 100)
	for rows.Next() {
		incident, err := scanIncidentRow(rows)
		if err != nil {
			return nil, err
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate incidents failed: %w", err)
	}

	return incidents, nil
}

func (p *PGPool) GetIncidentByID(ctx context.Context, id string) (*models.Incident, error) {
	query := `
SELECT id, service_name, type, status, raw_context, ai_summary, created_at, resolved_at
FROM incidents
WHERE id = $1
`
	row := p.Pool.QueryRow(ctx, query, id)

	var (
		incident        models.Incident
		rawContextBytes []byte
		resolvedAt      *time.Time
	)
	if err := row.Scan(
		&incident.ID,
		&incident.ServiceName,
		&incident.Type,
		&incident.Status,
		&rawContextBytes,
		&incident.AISummary,
		&incident.CreatedAt,
		&resolvedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get incident by id failed: %w", err)
	}

	incident.ResolvedAt = resolvedAt
	incident.RawContext = map[string]any{}
	if len(rawContextBytes) > 0 {
		if err := json.Unmarshal(rawContextBytes, &incident.RawContext); err != nil {
			return nil, fmt.Errorf("unmarshal incident raw_context failed: %w", err)
		}
	}

	return &incident, nil
}

func scanIncidentRow(rows pgx.Rows) (models.Incident, error) {
	var (
		incident        models.Incident
		rawContextBytes []byte
		resolvedAt      *time.Time
	)

	if err := rows.Scan(
		&incident.ID,
		&incident.ServiceName,
		&incident.Type,
		&incident.Status,
		&rawContextBytes,
		&incident.AISummary,
		&incident.CreatedAt,
		&resolvedAt,
	); err != nil {
		return models.Incident{}, fmt.Errorf("scan incident failed: %w", err)
	}

	incident.ResolvedAt = resolvedAt
	incident.RawContext = map[string]any{}
	if len(rawContextBytes) > 0 {
		if err := json.Unmarshal(rawContextBytes, &incident.RawContext); err != nil {
			return models.Incident{}, fmt.Errorf("unmarshal incident raw_context failed: %w", err)
		}
	}

	return incident, nil
}
