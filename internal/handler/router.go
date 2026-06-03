package handler

import (
	"log/slog"
	"time"

	"github.com/CoverOnes/marketplace/internal/platform/health"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// RouterConfig holds all handler-level dependencies.
type RouterConfig struct {
	ListingSvc *service.ListingService
	BidSvc     *service.BidService
	Pool       *pgxpool.Pool
	Redis      *redis.Client // may be nil in dev
}

// NewRouter builds and returns the configured Gin engine.
//
// CORS policy: CORS is intentionally NOT applied at this internal service layer.
// marketplace is reached only via the API gateway, which owns all browser-facing
// CORS handling. Adding permissive CORS here would widen the attack surface without
// benefit (CONVENTIONS §9 positions CORS after the access-log in the chain but
// the gateway/edge handles it before requests reach this service).
func NewRouter(cfg RouterConfig) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.SetTrustedProxies(nil) //nolint:errcheck // nil proxy list disables proxy trust; gin docs confirm error is always nil for nil argument

	// Global middleware chain (order per CONVENTIONS §9).
	r.Use(middleware.Recover())
	r.Use(middleware.RequestID())
	r.Use(middleware.SecurityHeaders())
	r.Use(accessLogger())

	// Health endpoints — registered BEFORE the rate limiter so that liveness /
	// readiness probes are never rate-limited.
	h := health.NewHandler(cfg.Pool)
	r.GET("/healthz", h.Liveness)
	r.GET("/readyz", h.Readiness)

	// Rate limiter — 120 req/min per IP for all API routes below.
	ipRL := middleware.NewIPRateLimiter(cfg.Redis, 120, time.Minute)
	r.Use(ipRL.Handler())

	// All API routes require a valid identity (gateway-injected X-User-Id).
	// RequireValidIdentity is applied as a group-level middleware.
	listingH := NewListingHandler(cfg.ListingSvc)
	bidH := NewBidHandler(cfg.BidSvc)

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())

	// Listings — Tier>=1 for browse, Tier>=2 for create/update.
	// /listings/search is registered before /listings/:id so the static segment
	// takes precedence over the param route.
	api.GET("/listings", middleware.RequireTier(1), listingH.List)
	api.GET("/listings/search", middleware.RequireTier(1), listingH.Search)
	api.GET("/listings/:id", middleware.RequireTier(1), listingH.GetByID)
	api.POST("/listings", middleware.RequireTier(2), listingH.Create)
	api.PATCH("/listings/:id", middleware.RequireTier(2), listingH.Update)

	// Bids — all Tier>=2.
	api.POST("/listings/:id/bids", middleware.RequireTier(2), bidH.CreateBid)
	api.GET("/listings/:id/bids", middleware.RequireTier(2), bidH.ListBidsForListing)
	api.GET("/bids", middleware.RequireTier(2), bidH.ListMyBids)
	api.POST("/bids/:id/accept", middleware.RequireTier(2), bidH.AcceptBid)
	api.POST("/bids/:id/reject", middleware.RequireTier(2), bidH.RejectBid)
	api.POST("/bids/:id/withdraw", middleware.RequireTier(2), bidH.WithdrawBid)

	return r
}

// accessLogger returns a minimal slog-based access-log middleware.
// Health probe paths (/healthz, /readyz) are excluded to keep logs noise-free
// and consistent with gateway/user service behavior.
func accessLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/healthz" || path == "/readyz" {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()
		slog.Info(
			"http",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString("request_id"),
		)
	}
}
