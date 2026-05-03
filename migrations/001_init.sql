-- Migration: 001_init.sql
-- Run this once against your PostgreSQL database to initialize the schema.
-- In production you'd use a migration tool (golang-migrate, goose) — for now
-- we run this manually or via Docker Compose's init scripts.

-- ============================================================
-- EXTENSIONS
-- ============================================================

-- uuid-ossp gives us uuid_generate_v4() for generating UUIDs in SQL.
-- We'll generate UUIDs in Go too, but this is useful for manual inserts/testing.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- ============================================================
-- TABLE: log_events
-- ============================================================
-- This is the raw, append-only event store. Every ingested log goes here.
-- Think of it as the source of truth — the worker reads from Redis first
-- for speed, but everything is durably persisted here.
--
-- Why append-only? Because logs are facts about the past. You never
-- "update" a log entry — you append new ones. This makes the table
-- safe to shard, replicate, and archive.

CREATE TABLE IF NOT EXISTS log_events (
    id           UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    service_name TEXT        NOT NULL,
    level        TEXT        NOT NULL CHECK (level IN ('INFO', 'WARN', 'ERROR')),
    message      TEXT        NOT NULL,
    latency_ms   NUMERIC(10, 3) NOT NULL DEFAULT 0,  -- allows sub-ms precision
    status_code  INTEGER     NOT NULL DEFAULT 0,
    timestamp    TIMESTAMPTZ NOT NULL DEFAULT NOW(),  -- TIMESTAMPTZ stores UTC + offset
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index on (service_name, timestamp) because our most common query pattern is:
-- "give me all events for service X in time range [start, end]"
-- A composite index on both columns serves this query efficiently.
-- Without this, every metric aggregation would be a full table scan.
CREATE INDEX IF NOT EXISTS idx_log_events_service_time
    ON log_events (service_name, timestamp DESC);

-- Index on level for filtering (e.g., "count of ERRORs per service")
CREATE INDEX IF NOT EXISTS idx_log_events_level
    ON log_events (level);

-- ============================================================
-- TABLE: log_metrics
-- ============================================================
-- Stores the aggregated output of each worker cycle.
-- One row = one service's metrics for one 30-second window.
--
-- Why store pre-aggregated metrics instead of querying log_events each time?
-- Because at scale, log_events could have millions of rows. Computing p95
-- latency on-demand across millions of rows is slow. Pre-aggregating every
-- 30s keeps dashboard/API queries fast — they read a small metrics table.
-- This is the same principle behind time-series databases (InfluxDB, TimescaleDB).

CREATE TABLE IF NOT EXISTS log_metrics (
    id             UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    service_name   TEXT        NOT NULL,
    window_start   TIMESTAMPTZ NOT NULL,
    window_end     TIMESTAMPTZ NOT NULL,
    p95_latency_ms NUMERIC(10, 3) NOT NULL,
    error_rate     NUMERIC(5, 4)  NOT NULL,  -- e.g., 0.0750 = 7.50%
    total_events   INTEGER     NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index on service + window for time-range queries from the API
CREATE INDEX IF NOT EXISTS idx_log_metrics_service_window
    ON log_metrics (service_name, window_start DESC);

-- ============================================================
-- TABLE: incidents
-- ============================================================
-- Created when the worker detects an anomaly (error rate >5% or latency >2x baseline).
--
-- Key design decision: raw_context is JSONB, not a fixed set of columns.
-- When an anomaly fires, we want to capture a full snapshot of the state:
-- which metrics triggered it, the baseline vs actual values, affected endpoints, etc.
-- This changes as the system evolves. JSONB lets us store any shape of data
-- without writing a new migration every time we add a field.
--
-- JSONB vs JSON in PostgreSQL:
--   - JSON stores the raw text as-is (preserves whitespace, key order)
--   - JSONB parses and stores in a binary format — supports indexing with GIN,
--     faster to query, slightly slower to write. JSONB is almost always preferred.

CREATE TABLE IF NOT EXISTS incidents (
    id           UUID           PRIMARY KEY DEFAULT uuid_generate_v4(),
    service_name TEXT           NOT NULL,
    type         TEXT           NOT NULL,   -- e.g., 'high_error_rate', 'high_latency'
    status       TEXT           NOT NULL DEFAULT 'open'
                                CHECK (status IN ('open', 'resolved')),
    raw_context  JSONB          NOT NULL DEFAULT '{}',
    ai_summary   TEXT           NOT NULL DEFAULT '',  -- populated after OpenAI call
    created_at   TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    resolved_at  TIMESTAMPTZ    -- NULL until resolved; no DEFAULT needed
);

-- GIN index on raw_context allows fast queries like:
--   WHERE raw_context @> '{"service_name": "payment-service"}'
-- GIN (Generalized Inverted Index) is the right index type for JSONB —
-- it indexes every key-value pair inside the JSON document.
CREATE INDEX IF NOT EXISTS idx_incidents_raw_context
    ON incidents USING GIN (raw_context);

-- Index on status — common filter: "show me all open incidents"
CREATE INDEX IF NOT EXISTS idx_incidents_status
    ON incidents (status);

-- Composite index for the most common dashboard query:
-- "open incidents for service X ordered by time"
CREATE INDEX IF NOT EXISTS idx_incidents_service_status
    ON incidents (service_name, status, created_at DESC);