# LogSense

## Project Overview

LogSense is a log-ingestion and incident-detection service designed to turn high-volume application logs into actionable incident records. It solves the gap between raw logs and operational response by ingesting events, aggregating service metrics, detecting anomalies, and attaching AI-readable incident context.

Current and target stack:
- Go 1.24 (`go.mod`) with Gin for the HTTP API; Docker image builds with Go 1.24 (see editor note below if `gopls` uses Go 1.22)
- PostgreSQL for durable event, metric, and incident storage
- Redis List as the ingest queue between API and worker
- OpenAI GPT-4o-mini (planned) for incident summarization
- Docker Compose for local multi-service orchestration
- Railway as the intended deployment target

## Architecture & Core Flow

LogSense follows a producer-consumer pipeline:
1. Client sends `POST /api/v1/ingest`.
2. API validates and enqueues event payload into a Redis List (`LPUSH`).
3. Background worker drains queue in batches (`LRANGE + LTRIM` pipeline).
4. Worker bulk-persists events to `log_events` and computes window metrics (`p95`, error rate).
5. Worker runs anomaly rules and creates `incidents` rows when thresholds are crossed.
6. New incidents are summarized by OpenAI GPT-4o-mini (planned Day 4).
7. API serves incident data via `GET /api/v1/incidents` and `GET /api/v1/incidents/:id`.

```text
Client
  |
  v
POST /api/v1/ingest
  |
  v
Redis List Queue (logsense:queue:log_events)
  |
  v
Background Worker (30s tick)
  |-------------------------------|
  |                               |
  v                               v
PostgreSQL: log_events/log_metrics  Anomaly Rules -> incidents
                                        |
                                        v
                                OpenAI GPT-4o-mini (planned)
                                        |
                                        v
                         GET /api/v1/incidents (+ /:id)
```

## Project Structure

Complete current project structure (excluding `.git` internals):

- `cmd/` - Application entrypoint package.
- `cmd/server/` - HTTP server bootstrap package.
- `cmd/server/main.go` - Initializes env/dependencies, starts worker and Gin server, handles graceful shutdown.
- `internal/` - Private application modules.
- `internal/ai/` - AI integration package.
- `internal/ai/openai.go` - Placeholder package file for upcoming OpenAI client integration.
- `internal/api/` - HTTP routing and handlers.
- `internal/api/router.go` - Defines all API routes and `501` placeholders for unfinished endpoints.
- `internal/api/handler.go` - Handler dependency injection and implemented `/health` endpoint.
- `internal/db/` - Database/cache clients and queue primitives.
- `internal/db/postgres.go` - PostgreSQL pool bootstrap and lifecycle (`NewPostgres`, `Ping`, `Close`).
- `internal/db/queries.go` - PostgreSQL query methods for log events, metrics, and incidents used by worker/API.
- `internal/db/redis.go` - Redis client setup, queue push/drain, and health helpers.
- `internal/models/` - Domain and API payload models.
- `internal/models/models.go` - Typed structs/enums for log events, metrics, incidents, and ingest request.
- `internal/worker/` - Background processing loop.
- `internal/worker/worker.go` - Worker ticker loop with queue drain, aggregation, anomaly detection, and incident creation logic.
- `migrations/` - SQL schema migrations.
- `migrations/001_init.sql` - Initializes `log_events`, `log_metrics`, `incidents`, and supporting indexes/extensions.
- `.gitignore` - Ignore rules for binaries, env files, editor artifacts, and local volume data.
- `docker-compose.yml` - Local orchestration for app + PostgreSQL + Redis with health checks.
- `Dockerfile` - Multi-stage build for small runtime image and non-root execution.
- `go.mod` - Module definition and direct/indirect dependencies.
- `go.sum` - Dependency checksums for reproducible builds.
- `README.md` - Project documentation.
- `.vscode/settings.json` - Workspace Go tooling env (`GOTOOLCHAIN=auto` for `gopls` when local `go` is older than `go.mod`).
- `.env.example` - Environment template file (currently present but empty, should be populated).

## Database Schema

### `log_events`
Append-only source-of-truth table for ingested logs.

Columns:
- `id UUID PRIMARY KEY DEFAULT uuid_generate_v4()`
- `service_name TEXT NOT NULL`
- `level TEXT NOT NULL CHECK (level IN ('INFO', 'WARN', 'ERROR'))`
- `message TEXT NOT NULL`
- `latency_ms NUMERIC(10,3) NOT NULL DEFAULT 0`
- `status_code INTEGER NOT NULL DEFAULT 0`
- `timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW()`
- `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`

Indexes:
- `idx_log_events_service_time` on `(service_name, timestamp DESC)` for service/time-window scans
- `idx_log_events_level` on `(level)` for severity filtering

### `log_metrics`
Pre-aggregated worker output per service per time window.

Columns:
- `id UUID PRIMARY KEY DEFAULT uuid_generate_v4()`
- `service_name TEXT NOT NULL`
- `window_start TIMESTAMPTZ NOT NULL`
- `window_end TIMESTAMPTZ NOT NULL`
- `p95_latency_ms NUMERIC(10,3) NOT NULL`
- `error_rate NUMERIC(5,4) NOT NULL`
- `total_events INTEGER NOT NULL`
- `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`

Indexes:
- `idx_log_metrics_service_window` on `(service_name, window_start DESC)`

### `incidents`
Anomaly records created by worker rules and later enriched with AI summary.

Columns:
- `id UUID PRIMARY KEY DEFAULT uuid_generate_v4()`
- `service_name TEXT NOT NULL`
- `type TEXT NOT NULL`
- `status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved'))`
- `raw_context JSONB NOT NULL DEFAULT '{}'`
- `ai_summary TEXT NOT NULL DEFAULT ''`
- `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`
- `resolved_at TIMESTAMPTZ NULL`

Indexes:
- `idx_incidents_raw_context` GIN on `(raw_context)` for JSONB containment queries
- `idx_incidents_status` on `(status)` for open/resolved filtering
- `idx_incidents_service_status` on `(service_name, status, created_at DESC)` for dashboard queries

Key schema decisions:
- `JSONB` in `incidents.raw_context` keeps anomaly payload extensible without frequent migrations.
- `TIMESTAMPTZ` is used everywhere for timezone-safe, UTC-friendly operational data.
- Composite indexes align with dominant read patterns (service + time window/status).
- `log_events` is append-only to preserve immutable event history and simplify ingestion semantics.
- GIN on `incidents.raw_context` enables performant ad hoc JSON filtering.

## API Endpoints

Routes defined in `internal/api/router.go`:

- `GET /health` - **Implemented** (`200` when dependencies are healthy, `503` when degraded)
- `POST /api/v1/ingest` - **Implemented** (`202 Accepted`, validates payload and enqueues to Redis)
- `GET /api/v1/metrics` - **Stubbed** (`501 Not Implemented`)
- `GET /api/v1/incidents` - **Stubbed** (`501 Not Implemented`)
- `GET /api/v1/incidents/:id` - **Stubbed** (`501 Not Implemented`)
- `POST /api/v1/incidents/:id/resolve` - **Stubbed** (`501 Not Implemented`)

## Day-by-Day Build Plan

- Day 1 (**DONE**): project structure, DB schema, Docker Compose, `/health` endpoint, graceful shutdown, dependency injection pattern.
- Day 2 (**DONE, tested**): `POST /ingest` handler, Redis `LPUSH`, worker drain logic (`LRANGE+LTRIM` pipeline), bulk insert into `log_events`, p95 latency aggregation.
- Day 3 (**TODO**): anomaly detection (error rate `>5%`, latency `>2x` baseline), incident creation with JSONB `raw_context`, `GET /incidents`, `GET /incidents/:id`, `GET /metrics`.
- Day 4 (**TODO**): OpenAI GPT-4o-mini summarization on new incidents, `POST /incidents/:id/resolve`, full Docker Compose bring-up, Railway deployment.

## How to Run Locally

1. Create env file:
   - If `.env.example` exists: `cp .env.example .env`
   - `.env.example` is currently empty, so populate `.env` manually with `DATABASE_URL`, `REDIS_URL`, and `OPENAI_API_KEY`.
2. Start dependencies:
   - `docker compose up -d postgres redis`
3. Run the API:
   - `go run ./cmd/server`
4. Verify health:
   - `curl -s http://localhost:8080/health`
5. Expected shape:
   - `{"status":"ok","timestamp":"...","services":{"postgres":"healthy","redis":"healthy"}}`

### Cursor / `gopls` (packages.Load / `go list`)

If the editor reports `go.mod requires go >= 1.24 (running go 1.22; GOTOOLCHAIN=local)`, reload the window after opening this repo: `.vscode/settings.json` sets `GOTOOLCHAIN=auto` for Go tooling so Go 1.22 can install the required toolchain. If it persists, remove a global `GOTOOLCHAIN=local` from your shell profile, or install Go 1.24+ and ensure `/usr/local/go/bin` is first on `PATH`.

## Key Design Decisions

- `LRANGE + LTRIM` drain pipeline: batches queue reads/removals in one Redis round-trip to reduce race risk between read and trim.
- Dependency injection via structs: `Handler` and `Worker` receive explicit dependencies, avoiding globals and improving testability.
- Multi-stage Dockerfile: compiles in builder stage and ships minimal runtime image for smaller deploy artifacts.
- Graceful shutdown on `SIGTERM`/`SIGINT`: server drains inflight requests and worker exits via shared cancellable context.
- Redis List as queue: simple, fast producer-consumer primitive suitable for current single-worker phase.
- Pre-aggregated metrics table: worker computes p95/error-rate periodically so API reads stay cheap at scale.

## Current Status

Day 1 and Day 2 are complete and tested end-to-end.

Validation completed:
- `POST /api/v1/ingest` accepts events and enqueues them in Redis.
- Worker drains queue on tick (`LLEN` observed moving from `10` to `0` after processing).
- Processed events are persisted in `log_events`.
- Aggregates are persisted in `log_metrics` (including p95/error rate).
- Incident dedup is active (`high_error_rate` incident remains open without duplicate creation).

Immediate next step:
- Start Day 3 endpoints and query paths: `GET /api/v1/metrics`, `GET /api/v1/incidents`, and `GET /api/v1/incidents/:id`.
