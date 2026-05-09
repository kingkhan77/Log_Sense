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
	"github.com/shahzad/logsense/internal/ai"
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

	// --- Postgres ---
	pg, err := db.NewPostgres(ctx)
	if err != nil {
		log.Fatalf("[main] failed to connect to postgres: %v", err)
	}
	defer pg.Close()
	log.Println("[main] postgres connected")

	// --- Redis ---
	redis, err := db.NewRedis(ctx)
	if err != nil {
		log.Fatalf("[main] failed to connect to redis: %v", err)
	}
	defer redis.Close()
	log.Println("[main] redis connected")

	// --- AI Summarizer (optional) ---
	// The worker depends on ai.Summarizer — an interface — not a concrete client.
	// To switch providers, change what you construct here. Nothing else changes.
	//
	// Currently: we try OpenAI first, then Gemini fallback if OpenAI fails.
	var summarizer ai.Summarizer
	providers := make([]ai.Summarizer, 0, 2)

	openaiClient, err := ai.NewOpenAIClient()
	if err != nil {
		log.Printf("[main] OpenAI unavailable: %v", err)
	} else {
		providers = append(providers, openaiClient)
	}

	geminiClient, err := ai.NewGeminiClient()
	if err != nil {
		log.Printf("[main] Gemini unavailable: %v", err)
	} else {
		providers = append(providers, geminiClient)
	}

	summarizer = ai.NewFallbackSummarizer(providers...)
	if summarizer == nil {
		log.Printf("[main] WARNING: no AI provider available — incident summarization disabled")
	} else {
		log.Printf("[main] AI summarizer ready (provider chain: %s)", summarizer.Provider())
	}

	// --- Worker ---
	// summarizer may be nil — worker handles that gracefully.
	w := worker.New(pg, redis, summarizer, 30*time.Second)
	go w.Start(ctx)
	log.Println("[main] background worker started")

	// --- HTTP server ---
	handler := api.NewHandler(pg, redis)
	router := api.NewRouter(handler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("[main] HTTP server listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[main] server error: %v", err)
		}
	}()

	// Graceful shutdown on SIGTERM / SIGINT
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("[main] shutdown signal received — draining connections...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] forced shutdown: %v", err)
	}

	cancel() // stop the worker
	log.Println("[main] shutdown complete")
}
