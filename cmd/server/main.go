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
	"github.com/CoverOnes/marketplace/internal/outbox"
	"github.com/CoverOnes/marketplace/internal/platform/logger"
	"github.com/CoverOnes/marketplace/internal/recommendation"
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
	// IMPORTANT (M-1): This pool does NOT register the pgvector codec.
	// Any handler or service that calls EmbeddingStore MUST use postgres.NewEmbeddingPool
	// instead — it registers the pgvector codec via an AfterConnect hook. Using this plain
	// pool with EmbeddingStore will produce "unknown type" scan errors at runtime.
	// TODO: when wiring an EmbeddingStore consumer, replace or supplement this pool with
	// postgres.NewEmbeddingPool(ctx, cfg.PostgresDSN, cfg.PostgresSchema, ...).
	pool, err := postgres.NewPool(ctx, cfg.PostgresDSN, cfg.PostgresSchema, postgres.PoolOptions{
		MaxConns: int32(cfg.DBMaxConns),
		MinConns: int32(cfg.DBMinConns),
	})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}

	defer pool.Close()

	slog.Info("postgres connected")

	// Migrations are applied by `task migrate` in dev-stack (or the equivalent
	// operator step in production) — NOT on boot. Running golang-migrate at boot
	// without the search_path=marketplace,public parameter would attempt to write
	// schema_migrations into the public schema, which svc_marketplace has no
	// privileges on (SQLSTATE 42501). The Taskfile migrate step runs as svc_marketplace
	// with search_path set, so schema_migrations lands in the marketplace schema.

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
	bidOutboxTxManager := postgres.NewBidOutboxTxManager(pool)

	// Tender store layer.
	tenderRoleStore := postgres.NewTenderRoleStore(pool)
	tenderCollabStore := postgres.NewTenderCollaboratorStore(pool)
	tenderMilestoneStore := postgres.NewTenderMilestoneStore(pool)
	tenderTxManager := postgres.NewTenderTxManager(pool)
	outboxTxManager := postgres.NewOutboxTxManager(pool)
	milestoneTxManager := postgres.NewMilestoneTxManager(pool)

	// Outbox store (for poller housekeeping — pool-backed).
	outboxStore := postgres.NewOutboxStore(pool)

	// Attachment store.
	attachmentStore := postgres.NewListingAttachmentStore(pool)

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

	// File S2S client: marketplace calls file service to register and presign attachments.
	// When MARKETPLACE_FILE_BASE_URL is empty, S2S calls are skipped (local dev).
	// The file service must have "marketplace" mapped to FILE_SERVICE_TOKEN in its S2S ACL
	// with entityType "listing" permitted (deploy requirement documented in PR notes).
	var fileClient client.FileClient
	if cfg.FileBaseURL != "" {
		fileClient = client.NewHTTPFileClient(cfg.FileBaseURL, cfg.FileServiceID, cfg.FileServiceToken)
		slog.Info("file client configured", "base_url", cfg.FileBaseURL, "service_id", cfg.FileServiceID)
	} else {
		slog.Warn("MARKETPLACE_FILE_BASE_URL not set; file S2S calls are disabled (dev mode)")
	}

	// Service layer.
	listingSvc := service.NewListingService(listingStore)
	bidSvc := service.NewBidService(bidStore, listingStore, awardStore, txManager, bidOutboxTxManager, publisher, workspaceClient)
	tenderSvc := service.NewTenderService(
		listingStore,
		tenderRoleStore,
		tenderCollabStore,
		tenderMilestoneStore,
		tenderTxManager,
		outboxTxManager,
		milestoneTxManager,
		workspaceClient,
		publisher,
	)
	// fileClient may be nil when FILE_BASE_URL is unset (local dev — S2S calls are skipped).
	attachmentSvc := service.NewAttachmentService(attachmentStore, listingStore, tenderCollabStore, fileClient)

	// Outbox poller — start only when Redis is available; interval from env (default 2s).
	// The poller goroutine is canceled on graceful shutdown via pollerCtx.
	pollerCtx, pollerCancel := context.WithCancel(ctx)
	defer pollerCancel()

	startOutboxPoller(pollerCtx, redisClient, outboxStore)

	// Recommendation retention runner — deletes ai_recommendation rows older
	// than 30 days on a 24-hour cycle (backend-security §1.3 TTL requirement).
	// Uses pollerCtx so it is canceled on graceful shutdown alongside the poller.
	recStore := postgres.NewRecommendationStore(pool)
	retentionRunner := recommendation.NewRetentionRunner(recStore, 0) // 0 = 24h default

	go retentionRunner.Run(pollerCtx)

	slog.Info("recommendation retention runner started")

	// Router.
	r := handler.NewRouter(&handler.RouterConfig{
		ListingSvc:          listingSvc,
		BidSvc:              bidSvc,
		TenderSvc:           tenderSvc,
		AttachmentSvc:       attachmentSvc,
		Pool:                pool,
		Redis:               redisClient,
		GatewayHMACSecret:   cfg.GatewayHMACSecret,
		UserRateLimitPerMin: cfg.UserRateLimitPerMin,
		UserRateLimitBurst:  cfg.UserRateLimitBurst,
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

// startOutboxPoller starts the transactional outbox poller when redisClient is non-nil.
// It reads the poll interval from MARKETPLACE_OUTBOX_POLL_INTERVAL (default 2s).
// The poller goroutine runs until ctx is canceled.
func startOutboxPoller(ctx context.Context, rdb *redis.Client, outboxStore *postgres.OutboxStore) {
	if rdb == nil {
		slog.Warn("redis not configured; outbox poller disabled — events will not be delivered")

		return
	}

	interval := outboxPollInterval()
	rawPublisher := outbox.NewRedisRawPublisher(rdb)
	poller := outbox.NewPoller(outboxStore, rawPublisher, interval)

	go func() {
		poller.Run(ctx)
	}()

	slog.Info("outbox poller started", "interval", interval.String())
}

// outboxPollInterval parses MARKETPLACE_OUTBOX_POLL_INTERVAL and returns the
// configured duration, or the default 2s when the env var is absent or invalid.
func outboxPollInterval() time.Duration {
	const defaultInterval = 2 * time.Second

	v := os.Getenv("MARKETPLACE_OUTBOX_POLL_INTERVAL")
	if v == "" {
		return defaultInterval
	}

	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		// Truncate the raw value to prevent log-line flooding (G706: operator env, not user input).
		truncated := v
		if len(truncated) > 64 {
			truncated = truncated[:64]
		}

		//nolint:gosec // G706 false positive: truncated is an operator-controlled env var, not user input; already capped to 64 chars
		slog.Warn("invalid MARKETPLACE_OUTBOX_POLL_INTERVAL; using default 2s", "raw", truncated)

		return defaultInterval
	}

	return d
}
