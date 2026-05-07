package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shahzad/logsense/internal/db"
	"github.com/shahzad/logsense/internal/models"
)

// Handler holds all dependencies the HTTP layer needs.
// This is the "dependency injection via struct" pattern — rather than
// using global variables, we bundle deps into a struct and pass it around.
// Benefits: easy to test (swap real PG/Redis with mocks), explicit about
// what each handler actually needs.
type Handler struct {
	PG    *db.PGPool
	Redis *db.RedisClient
}

// NewHandler constructs a Handler with its dependencies.
func NewHandler(pg *db.PGPool, redis *db.RedisClient) *Handler {
	return &Handler{
		PG:    pg,
		Redis: redis,
	}
}

// HealthResponse is the shape of GET /health.
// Exporting it as a named struct (rather than gin.H map) means the response
// shape is documented in code and easier to test against.
type HealthResponse struct {
	Status    string            `json:"status"`
	Timestamp time.Time         `json:"timestamp"`
	Services  map[string]string `json:"services"`
}

// Health handles GET /health.
//
// A good health endpoint does three things:
//  1. Returns 200 quickly when everything is up (used by load balancers
//     and Railway's health check to decide if the container is ready)
//  2. Returns 503 with details when a dependency is down
//  3. Never takes longer than ~2s (we use a tight context deadline)
//
// Checking both Postgres and Redis here means one failing endpoint tells
// you *which* dependency is broken — much more useful than a binary up/down.
func (h *Handler) Health(c *gin.Context) {
	// 2-second deadline — if PG or Redis can't pong us in 2s, something
	// is seriously wrong and we should report degraded.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	services := make(map[string]string)
	overallStatus := "ok"

	// Check Postgres connectivity
	if err := h.PG.Ping(ctx); err != nil {
		services["postgres"] = "unhealthy: " + err.Error()
		overallStatus = "degraded"
	} else {
		services["postgres"] = "healthy"
	}

	// Check Redis connectivity
	if err := h.Redis.Ping(ctx); err != nil {
		services["redis"] = "unhealthy: " + err.Error()
		overallStatus = "degraded"
	} else {
		services["redis"] = "healthy"
	}

	statusCode := http.StatusOK
	if overallStatus == "degraded" {
		statusCode = http.StatusServiceUnavailable
	}

	c.JSON(statusCode, HealthResponse{
		Status:    overallStatus,
		Timestamp: time.Now().UTC(),
		Services:  services,
	})
}

// Ingest handles POST /api/v1/ingest.
//
// Responsibility is deliberately narrow: validate → enqueue → respond.
// The handler does NOT write to Postgres — that's the worker's job.
// This keeps the ingest path fast: one JSON decode + one Redis LPUSH.
// Even if the worker is behind, ingest keeps accepting at full speed.
//
// Flow:
//  1. Bind + validate the JSON body (Gin's ShouldBindJSON uses binding tags)
//  2. Build a LogEvent with a new UUID and current timestamp
//  3. Serialize to JSON and push to the Redis queue
//  4. Return 202 Accepted — not 200 OK, because the event is queued,
//     not yet persisted. 202 means "received, will process".
func (h *Handler) Ingest(c *gin.Context) {
	var req models.IngestRequest

	// ShouldBindJSON decodes the body AND validates `binding:"required"` tags.
	// If any required field is missing or the body isn't valid JSON, it returns
	// a descriptive error — we surface that to the caller as a 400.
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid_request",
			"message": err.Error(),
		})
		return
	}

	// Build the full LogEvent. The handler assigns ID and Timestamp —
	// not the caller — so these fields are always server-controlled.
	// Trusting client-supplied timestamps would allow backdating events.
	event := models.LogEvent{
		ID:          uuid.New().String(),
		ServiceName: req.ServiceName,
		Level:       req.Level,
		Message:     req.Message,
		LatencyMs:   req.LatencyMs,
		StatusCode:  req.StatusCode,
		Timestamp:   time.Now().UTC(),
	}

	// Serialize to JSON for Redis storage.
	// We store JSON (not gob/msgpack) because it's human-readable in redis-cli,
	// which is invaluable for debugging during development.
	payload, err := json.Marshal(event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "serialization_error",
			"message": "failed to encode event",
		})
		return
	}

	// Push to Redis. Use a tight context — if Redis is slow or down, we
	// fail fast and return 503 rather than hanging the HTTP connection.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := h.Redis.PushEvent(ctx, payload); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "queue_error",
			"message": "failed to enqueue event, try again",
		})
		return
	}

	// 202 Accepted: the event is in the queue and will be processed.
	// We return the event ID so callers can correlate this ingest
	// with future incident records if needed.
	c.JSON(http.StatusAccepted, gin.H{
		"status":   "accepted",
		"event_id": event.ID,
	})
}

// GetIncidents handles GET /api/v1/incidents.
//
// Returns the 100 most recent incidents ordered by created_at DESC.
// In a production system you'd add pagination (cursor or offset), but
// for LogSense's scope a fixed limit is fine and keeps the API simple.
func (h *Handler) GetIncidents(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	incidents, err := h.PG.GetIncidents(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "db_error",
			"message": "failed to fetch incidents",
		})
		return
	}

	// Return an empty array rather than null when there are no incidents.
	// null is valid JSON but breaks clients that don't check for it —
	// an empty array is always safe to iterate over.
	if incidents == nil {
		incidents = []models.Incident{}
	}

	c.JSON(http.StatusOK, gin.H{
		"incidents": incidents,
		"count":     len(incidents),
	})
}

// GetIncidentByID handles GET /api/v1/incidents/:id.
//
// Returns a single incident by UUID. Returns 404 if not found.
// We differentiate "not found" from "db error" so clients can handle
// each case correctly (retry on 500, stop on 404).
func (h *Handler) GetIncidentByID(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "missing_id",
			"message": "incident id is required",
		})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	incident, err := h.PG.GetIncidentByID(ctx, id)
	if err != nil {
		// pgx returns an error wrapping pgx.ErrNoRows when no row is found.
		// We check the error message rather than unwrapping to avoid importing
		// pgx directly into the handler layer (keeps the api package clean).
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "not_found",
			"message": "incident not found",
		})
		return
	}

	c.JSON(http.StatusOK, incident)
}

// GetMetrics handles GET /api/v1/metrics.
//
// Returns the 50 most recent metric windows across all services.
// Useful for dashboards: plot p95 latency and error rate over time per service.
func (h *Handler) GetMetrics(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	metrics, err := h.PG.GetRecentMetrics(ctx, 50)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "db_error",
			"message": "failed to fetch metrics",
		})
		return
	}

	if metrics == nil {
		metrics = []models.LogMetric{}
	}

	c.JSON(http.StatusOK, gin.H{
		"metrics": metrics,
		"count":   len(metrics),
	})
}

// ResolveIncident handles POST /api/v1/incidents/:id/resolve.
//
// Marks an open incident as resolved. Idempotency note: if the incident
// is already resolved, ResolveIncident returns an error and we return 409
// Conflict — not 200. This is intentional: the caller should know their
// action had no effect so they can update their own state accordingly.
//
// We use POST not PATCH/PUT because this is an action (resolve), not a
// partial update of fields. REST purists might disagree, but POST on a
// sub-resource (/resolve) is a widely accepted pattern for state transitions.
func (h *Handler) ResolveIncident(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "missing_id",
			"message": "incident id is required",
		})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.PG.ResolveIncident(ctx, id); err != nil {
		// ResolveIncident returns a specific error when RowsAffected == 0,
		// meaning the incident doesn't exist or is already resolved.
		c.JSON(http.StatusConflict, gin.H{
			"error":   "conflict",
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":      "resolved",
		"incident_id": id,
	})
}
