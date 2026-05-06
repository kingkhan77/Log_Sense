package api

import (
	"github.com/gin-gonic/gin"
)

// NewRouter wires all routes to their handlers and returns a configured engine.
//
// Separating router setup from main.go keeps the entry point clean and makes
// it easy to spin up a test server (httptest.NewServer) without importing main.
//
// We define all routes here even if the handlers aren't implemented yet —
// this gives us a complete picture of the API surface and lets us return
// meaningful 501 Not Implemented responses during development.
func NewRouter(h *Handler) *gin.Engine {
	// gin.New() gives us a bare engine without the default Logger and Recovery
	// middleware — we add them explicitly so we stay in control of what runs.
	r := gin.New()

	// Recovery catches panics in handlers and returns 500 instead of crashing.
	// In production you'd replace this with a custom recovery that logs to your
	// observability system (ironic, since LogSense IS that system).
	r.Use(gin.Recovery())

	// Logger middleware prints each request: method, path, status, latency.
	// We'll replace this with structured logging later in the series.
	r.Use(gin.Logger())

	// Health check — no auth, always public.
	// Railway uses this to determine container readiness.
	r.GET("/health", h.Health)

	// v1 API group — versioning from day one means we can introduce /v2
	// without breaking existing clients. This is a common production practice.
	v1 := r.Group("/api/v1")
	{
		// Ingest — receives log events and pushes to Redis queue.
		// Implemented in Day 2.
		v1.POST("/ingest", h.Ingest)

		// Metrics — returns aggregated metrics from the worker.
		// Implemented in Day 3.
		v1.GET("/metrics", h.notImplemented("GET /metrics — coming Day 3"))

		// Incidents CRUD — implemented in Day 3/4.
		incidents := v1.Group("/incidents")
		{
			incidents.GET("", h.notImplemented("GET /incidents — coming Day 3"))
			incidents.GET("/:id", h.notImplemented("GET /incidents/:id — coming Day 3"))
			incidents.POST("/:id/resolve", h.notImplemented("POST /incidents/:id/resolve — coming Day 4"))
		}
	}

	return r
}

// notImplemented returns a placeholder handler with a descriptive message.
// Much better than a 404 — tells future-you (and interviewers) the API was
// intentionally designed upfront, not discovered ad-hoc.
func (h *Handler) notImplemented(msg string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(501, gin.H{
			"error":   "not_implemented",
			"message": msg,
		})
	}
}