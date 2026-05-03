package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/shahzad/logsense/internal/api"
	"github.com/shahzad/logsense/internal/db"
	"github.com/shahzad/logsense/internal/worker"
)

func main() {
	// Load .env in development. In production (Railway), env vars are injected
	// by the platform — godotenv.Load() will simply return a non-fatal error
	// which we ignore. This is idiomatic: don't fail if .env is absent.
	if err := godotenv.Load(); err != nil {
		log.Println("[main] no .env file found — using environment variables")
	}

	// Root context for the entire application.
	// All goroutines (worker, server) receive a derived context.
	// When we cancel this (on SIGTERM), it cascades to all children.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Connect to dependencies ---

	pg, err := db.NewPostgres(ctx)
	if err != nil {
		log.Fatalf("[main] failed to connect to postgres: %v", err)
	}
	defer pg.Close()
	log.Println("[main] postgres connected")

	redis, err := db.NewRedis(ctx)
	if err != nil {
		log.Fatalf("[main] failed to connect to redis: %v", err)
	}
	defer redis.Close()
	log.Println("[main] redis connected")

	// --- Start background worker ---
	//
	// We launch the worker in its own goroutine so it doesn't block the
	// HTTP server from starting. The worker receives ctx — when we cancel
	// it on shutdown, the worker's select loop exits cleanly.
	w := worker.New(pg, redis, 30*time.Second)
	go w.Start(ctx)
	log.Println("[main] background worker started")

	// --- Start HTTP server ---

	handler := api.NewHandler(pg, redis)
	router := api.NewRouter(handler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,

		// These timeouts are critical for production. Without them:
		// - ReadTimeout: a slow client that never sends a body keeps
		//   the connection open forever, exhausting your goroutine pool.
		// - WriteTimeout: a handler that hangs keeps writing forever.
		// - IdleTimeout: keep-alive connections that sit idle drain resources.
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown via OS signal handling.
	//
	// The pattern:
	//   1. Start the server in a goroutine (so we can listen for signals below)
	//   2. Block on signal.NotifyContext until SIGTERM or SIGINT arrives
	//   3. Give in-flight requests up to 10s to finish (srv.Shutdown)
	//   4. Cancel the root context — tells the worker to stop
	//
	// This is the production standard for Go HTTP servers. Railway sends
	// SIGTERM when deploying a new version, so we need to handle it cleanly.
	go func() {
		log.Printf("[main] HTTP server listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[main] server error: %v", err)
		}
	}()

	// Block until we receive an interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("[main] shutdown signal received — draining connections...")

	// Give existing requests 10 seconds to complete before force-closing
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] HTTP server forced to shut down: %v", err)
	}

	// Cancel the root context — worker's select will hit ctx.Done() and exit
	cancel()

	log.Println("[main] shutdown complete")
}