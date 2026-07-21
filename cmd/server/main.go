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
	"noapp/internal/auth"
	"noapp/internal/telemetry"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	addr := env("APP_ADDR", ":8080")
	databaseURL := env("DATABASE_URL", "postgres://noapp:noapp@localhost:5432/noapp?sslmode=disable")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	environment := env("APP_ENV", "development")
	logger, shutdownLogger, err := telemetry.NewLogger(ctx, environment)
	if err != nil {
		slog.Error("initialize OpenTelemetry logs", "error", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = shutdownLogger(shutdownCtx)
	}()

	meterProvider, err := telemetry.NewMeterProvider(ctx, environment)
	if err != nil {
		slog.Error("initialize OpenTelemetry metrics", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = meterProvider.Shutdown(shutdownCtx)
	}()

	tracerProvider, err := telemetry.NewTracerProvider(ctx, environment)
	if err != nil {
		slog.Error("initialize OpenTelemetry traces", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = tracerProvider.Shutdown(shutdownCtx)
	}()

	shutdownProfiler, err := telemetry.NewProfiler(env("PYROSCOPE_SERVER_ADDRESS", ""), "noapp", environment)
	if err != nil {
		slog.Error("initialize continuous profiling", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := shutdownProfiler(); err != nil {
			slog.Error("stop continuous profiling", "error", err)
		}
	}()

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		slog.Error("parse database configuration", "error", err)
		os.Exit(1)
	}
	poolConfig.ConnConfig.Tracer = otelpgx.NewTracer()
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		slog.Error("create database pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := waitForDatabase(ctx, pool); err != nil {
		slog.Error("database unavailable", "error", err)
		os.Exit(1)
	}

	verifier, err := auth.NewVerifier(auth.Config{
		Issuer:   env("AUTH_ISSUER_URL", "http://localhost:8082/realms/noapp"),
		JWKSURL:  env("AUTH_JWKS_URL", "http://localhost:8082/realms/noapp/protocol/openid-connect/certs"),
		Audience: env("AUTH_AUDIENCE", "noapp-api"),
	})
	if err != nil {
		slog.Error("initialize token verifier", "error", err)
		os.Exit(1)
	}
	handler, err := app.New(pool, verifier, app.AuthUIConfig{
		Issuer:   env("AUTH_BROWSER_ISSUER_URL", "http://localhost:8082/realms/noapp"),
		ClientID: env("AUTH_BROWSER_CLIENT_ID", "noapp-web"),
	})
	if err != nil {
		slog.Error("initialize HTTP handler", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           otelhttp.NewHandler(handler, "HTTP request"),
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
