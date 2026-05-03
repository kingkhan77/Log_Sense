package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shahzad/logsense/internal/db"
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
//   1. Returns 200 quickly when everything is up (used by load balancers
//      and Railway's health check to decide if the container is ready)
//   2. Returns 503 with details when a dependency is down
//   3. Never takes longer than ~2s (we use a tight context deadline)
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