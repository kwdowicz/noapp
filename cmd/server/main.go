package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"noapp/internal/app"
	"noapp/internal/telemetry"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	addr := env("APP_ADDR", ":8080")
	databaseURL := env("DATABASE_URL", "postgres://noapp:noapp@localhost:5432/noapp?sslmode=disable")
	logger, shutdownLogs, err := telemetry.NewLogger(context.Background(), env("APP_ENV", "development"))
	if err != nil {
		slog.Error("initialize OpenTelemetry logs", "error", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownLogs(shutdownCtx); err != nil {
			slog.Error("flush OpenTelemetry logs", "error", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		slog.Error("create database pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := waitForDatabase(ctx, pool); err != nil {
		slog.Error("database unavailable", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           app.New(pool),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("server started", "address", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server stopped unexpectedly", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown", "error", err)
	}
}

func waitForDatabase(ctx context.Context, pool *pgxpool.Pool) error {
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		if err = pool.Ping(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
