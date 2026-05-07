package api

import (
	"github.com/gin-gonic/gin"
)

// NewRouter wires all routes to their handlers and returns a configured engine.
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
		v1.GET("/metrics", h.GetMetrics)

		// Incidents CRUD — implemented in Day 3/4.
		incidents := v1.Group("/incidents")
		{
			incidents.GET("", h.GetIncidents)
			incidents.GET("/:id", h.GetIncidentByID)
			incidents.POST("/:id/resolve", h.ResolveIncident)
		}
	}

	return r
}

// notImplemented returns a placeholder handler with a descriptive message.
// Much better than a 404 — tells future-you (and interviewers) the API was
// intentionally designed upfront, not discovered ad-hoc.
// kept fro future use
func (h *Handler) notImplemented(msg string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(501, gin.H{
			"error":   "not_implemented",
			"message": msg,
		})
	}
}
