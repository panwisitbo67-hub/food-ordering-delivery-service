package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"time"

	"food-ordering/delivery-service/internal/delivery"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	databaseURL := os.Getenv("DATABASE_URL")
	jwtSecret := os.Getenv("JWT_SECRET")
	internalKey := os.Getenv("INTERNAL_API_KEY")
	if databaseURL == "" || jwtSecret == "" || internalKey == "" {
		logger.Error("DATABASE_URL, JWT_SECRET and INTERNAL_API_KEY are required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	store, err := delivery.OpenPostgres(ctx, databaseURL)
	cancel()
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer store.DB.Close()
	migration, err := os.ReadFile("migrations/001_init.sql")
	if err != nil {
		logger.Error("migration file not found; run from project root", "error", err)
		os.Exit(1)
	}
	if err = store.Migrate(context.Background(), string(migration)); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
	timeout, err := time.ParseDuration(env("HTTP_TIMEOUT", "5s"))
	if err != nil || timeout <= 0 {
		logger.Error("invalid HTTP_TIMEOUT")
		os.Exit(1)
	}
	limit, err := strconv.Atoi(env("RATE_LIMIT_PER_MINUTE", "120"))
	if err != nil || limit <= 0 {
		logger.Error("invalid RATE_LIMIT_PER_MINUTE")
		os.Exit(1)
	}
	upstream := &delivery.HTTPUpstreams{
		Client:        &http.Client{Timeout: timeout},
		OrderURL:      env("ORDER_SERVICE_URL", "http://localhost:8083"),
		UserURL:       env("USER_SERVICE_URL", "http://localhost:8081"),
		RestaurantURL: env("RESTAURANT_SERVICE_URL", "http://localhost:8082"),
		InternalKey:   internalKey,
	}
	server := &http.Server{Addr: ":" + env("PORT", "8084"), Handler: delivery.NewServer(store, upstream, jwtSecret, internalKey, limit, logger).Handler(), ReadHeaderTimeout: 5 * time.Second}
	stop, done := signal.NotifyContext(context.Background(), os.Interrupt)
	defer done()
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	logger.Info("delivery service listening", "address", server.Addr)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
