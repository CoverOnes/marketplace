// Command server starts the CoverOnes marketplace microservice.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/config"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/handler"
	"github.com/CoverOnes/marketplace/internal/platform/logger"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/redis/go-redis/v9"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "perform a liveness check against /healthz and exit 0/1")
	flag.Parse()

	// Docker HEALTHCHECK mode: GET /healthz and exit immediately.
	if *healthcheck {
		if err := runHealthCheck(); err != nil {
			slog.Error("healthcheck failed", "err", err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// runHealthCheck issues a GET to the local /healthz endpoint.
func runHealthCheck() error {
	port := os.Getenv("MARKETPLACE_PORT")
	if port == "" {
		port = "8081"
	}

	url := fmt.Sprintf("http://127.0.0.1:%s/healthz", port)

	httpClient := &http.Client{Timeout: 2 * time.Second}

	resp, err := httpClient.Get(url) //nolint:noctx // healthcheck is a one-shot process; no request context needed
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close on healthcheck response

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Logger — JSON to stdout (CONVENTIONS §5).
	log := logger.New(cfg.LogLevel)
	slog.SetDefault(log)

	ctx := context.Background()

	// Postgres pool (CONVENTIONS §12).
	// cfg.PostgresSchema is "" by default (public schema); set MARKETPLACE_DB_SCHEMA
	// to isolate this service within a shared Aiven database.
	// cfg.DBMaxConns / cfg.DBMinConns default to 10 / 2; lower them when sharing a
	// small Aiven plan across multiple services (MARKETPLACE_DB_MAX_CONNS / _MIN_CONNS).
	pool, err := postgres.NewPool(ctx, cfg.PostgresDSN, cfg.PostgresSchema, postgres.PoolOptions{
		MaxConns: int32(cfg.DBMaxConns),
		MinConns: int32(cfg.DBMinConns),
	})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}

	defer pool.Close()

	slog.Info("postgres connected")

	// Redis client (optional — nil means noop publisher + in-process rate limiter).
	var redisClient *redis.Client

	var publisher events.Publisher

	if cfg.RedisURL != "" {
		opts, parseErr := redis.ParseURL(cfg.RedisURL)
		if parseErr != nil {
			return fmt.Errorf("parse redis url: %w", parseErr)
		}

		redisClient = redis.NewClient(opts)

		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		if pingErr := redisClient.Ping(pingCtx).Err(); pingErr != nil {
			slog.Warn("redis ping failed; event publishing and rate limiting will use noop/fallback", "err", pingErr)
			redisClient = nil
		} else {
			slog.Info("redis connected")
		}
	}

	if redisClient != nil {
		publisher = events.NewRedisPublisher(redisClient)
	} else {
		publisher = events.NewNoopPublisher()
	}

	// Store layer.
	listingStore := postgres.NewListingStore(pool)
	bidStore := postgres.NewBidStore(pool)
	awardStore := postgres.NewAwardStore(pool)
	txManager := postgres.NewTxManager(pool)

	// Tender store layer.
	tenderRoleStore := postgres.NewTenderRoleStore(pool)
	tenderCollabStore := postgres.NewTenderCollaboratorStore(pool)
	tenderMilestoneStore := postgres.NewTenderMilestoneStore(pool)
	tenderTxManager := postgres.NewTenderTxManager(pool)

	// Workspace S2S client (M-2 fix): marketplace calls workspace after AcceptBid
	// to create the contract with authoritative award values.
	// When MARKETPLACE_WORKSPACE_BASE_URL is empty, the call is skipped (local dev).
	var workspaceClient client.WorkspaceClient
	if cfg.WorkspaceBaseURL != "" {
		workspaceClient = client.NewHTTPWorkspaceClient(cfg.WorkspaceBaseURL, cfg.WorkspaceServiceToken)
		slog.Info("workspace client configured", "base_url", cfg.WorkspaceBaseURL)
	} else {
		slog.Warn("MARKETPLACE_WORKSPACE_BASE_URL not set; workspace contract creation is disabled")
	}

	// Service layer.
	listingSvc := service.NewListingService(listingStore)
	bidSvc := service.NewBidService(bidStore, listingStore, awardStore, txManager, publisher, workspaceClient)
	tenderSvc := service.NewTenderService(
		listingStore,
		tenderRoleStore,
		tenderCollabStore,
		tenderMilestoneStore,
		tenderTxManager,
		workspaceClient,
		publisher,
	)

	// Router.
	r := handler.NewRouter(handler.RouterConfig{
		ListingSvc:        listingSvc,
		BidSvc:            bidSvc,
		TenderSvc:         tenderSvc,
		Pool:              pool,
		Redis:             redisClient,
		GatewayHMACSecret: cfg.GatewayHMACSecret,
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("server starting", "addr", srv.Addr)

		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("server listen error", "err", listenErr)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("shutting down gracefully")

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
		return fmt.Errorf("server shutdown: %w", shutdownErr)
	}

	slog.Info("server stopped")

	return nil
}
