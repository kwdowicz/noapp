package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"noapp/internal/simulator"
	"noapp/internal/telemetry"
)

func main() {
	addr := env("SIMULATOR_ADDR", ":8081")
	target := env("TARGET_APP_URL", "http://localhost:8080")
	engine := simulator.NewEngine(target, simulator.OAuthConfig{
		TokenURL:     env("AUTH_TOKEN_URL", ""),
		ClientID:     env("AUTH_CLIENT_ID", ""),
		ClientSecret: env("AUTH_CLIENT_SECRET", ""),
	})
	server := &http.Server{
		Addr:              addr,
		Handler:           simulator.NewHandler(engine),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	environment := env("APP_ENV", "development")
	shutdownProfiler, err := telemetry.NewProfiler(env("PYROSCOPE_SERVER_ADDRESS", ""), "noapp-simulator", environment)
	if err != nil {
		slog.Error("initialize continuous profiling", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := shutdownProfiler(); err != nil {
			slog.Error("stop continuous profiling", "error", err)
		}
	}()
	go func() {
		slog.Info("traffic simulator started", "address", addr, "target", target)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("traffic simulator stopped unexpectedly", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	engine.Stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("traffic simulator graceful shutdown", "error", err)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
