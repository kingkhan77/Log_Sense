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

type Handler struct {
	PG    *db.PGPool
	Redis *db.RedisClient
}

func NewHandler(pg *db.PGPool, redis *db.RedisClient) *Handler {
	return &Handler{PG: pg, Redis: redis}
}

type HealthResponse struct {
	Status    string            `json:"status"`
	Timestamp time.Time         `json:"timestamp"`
	Services  map[string]string `json:"services"`
}

func (h *Handler) Health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	services := make(map[string]string)
	overallStatus := "ok"

	if err := h.PG.Ping(ctx); err != nil {
		services["postgres"] = "unhealthy: " + err.Error()
		overallStatus = "degraded"
	} else {
		services["postgres"] = "healthy"
	}

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

func (h *Handler) Ingest(c *gin.Context) {
	var req models.IngestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	event := models.LogEvent{
		ID:          uuid.New().String(),
		ServiceName: req.ServiceName,
		Level:       req.Level,
		Message:     req.Message,
		LatencyMs:   req.LatencyMs,
		StatusCode:  req.StatusCode,
		Timestamp:   time.Now().UTC(),
	}

	payload, err := json.Marshal(event)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "serialization_error", "message": "failed to encode event"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := h.Redis.PushEvent(ctx, payload); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "queue_error", "message": "failed to enqueue event"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"status": "accepted", "event_id": event.ID})
}