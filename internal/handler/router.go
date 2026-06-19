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
	ListingSvc       *service.ListingService
	BidSvc           *service.BidService
	TenderSvc        *service.TenderService
	AttachmentSvc    *service.AttachmentService
	VendorProfileSvc *service.VendorProfileService
	Pool             *pgxpool.Pool
	Redis            *redis.Client // may be nil in dev

	// GatewayHMACSecret is the §24.1 shared secret used to verify the
	// gateway-origin identity signature. Empty == dev posture (verification
	// disabled); config validation guarantees it is non-empty in non-dev.
	GatewayHMACSecret string

	// UserRateLimitPerMin is the per-authenticated-user rate limit (req/min).
	// Set to 0 to disable per-user rate limiting (e.g. local dev without Redis).
	UserRateLimitPerMin int

	// UserRateLimitBurst is the token-bucket burst for per-user limiting.
	// MUST be > 0 when UserRateLimitPerMin > 0.
	UserRateLimitBurst int
}

// NewRouter builds and returns the configured Gin engine.
//
// CORS policy: CORS is intentionally NOT applied at this internal service layer.
// marketplace is reached only via the API gateway, which owns all browser-facing
// CORS handling. Adding permissive CORS here would widen the attack surface without
// benefit (CONVENTIONS §9 positions CORS after the access-log in the chain but
// the gateway/edge handles it before requests reach this service).
func NewRouter(cfg *RouterConfig) *gin.Engine {
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
	tenderH := NewTenderHandler(cfg.TenderSvc)
	attachmentH := NewAttachmentHandler(cfg.AttachmentSvc)
	vendorProfileH := NewVendorProfileHandler(cfg.VendorProfileSvc)

	api := r.Group("/v1")
	// Defense-in-depth (§24.1): verify the gateway-origin HMAC signature BEFORE
	// RequireValidIdentity trusts any X-User-Id / X-Kyc-Tier / X-Account-Type /
	// X-Email-Verified header. When the secret is empty (dev) this is a no-op
	// passthrough, matching the gateway's dev signing-skip.
	api.Use(middleware.VerifyGatewaySignature(cfg.GatewayHMACSecret, cfg.Redis))
	api.Use(middleware.RequireValidIdentity())

	// Per-user token-bucket limiter — mounted AFTER VerifyGatewaySignature +
	// RequireValidIdentity so the key is always a gateway-verified UUID, never
	// attacker-controlled. Only enabled when UserRateLimitPerMin > 0.
	if cfg.UserRateLimitPerMin > 0 {
		userRL := middleware.NewUserRateLimiter(cfg.UserRateLimitPerMin, cfg.UserRateLimitBurst)
		api.Use(userRL.Handler())
		slog.Info(
			"per-user rate limiter enabled",
			"limit_per_min", cfg.UserRateLimitPerMin,
			"burst", cfg.UserRateLimitBurst,
		)
	}

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

	// Attachments — POST/DELETE require Tier>=2; GET requires Tier>=1.
	// :id is the listing ID; :attachmentId is the attachment UUID.
	api.POST("/listings/:id/attachments", middleware.RequireTier(2), attachmentH.Attach)
	api.GET("/listings/:id/attachments", middleware.RequireTier(1), attachmentH.List)
	api.GET("/listings/:id/attachments/:attachmentId/download-url", middleware.RequireTier(1), attachmentH.DownloadURL)
	api.DELETE("/listings/:id/attachments/:attachmentId", middleware.RequireTier(2), attachmentH.Detach)

	// Tender — owner-only role/milestone management (Tier>=2); collaborator apply/exit (Tier>=2).
	// Roles sub-resource under /listings/:id/tender/.
	api.POST("/listings/:id/tender/roles", middleware.RequireTier(2), tenderH.CreateRole)
	api.GET("/listings/:id/tender/roles", middleware.RequireTier(2), tenderH.ListRoles)
	api.POST("/listings/:id/tender/roles/:roleId/close", middleware.RequireTier(2), tenderH.CloseRole)
	// Milestones sub-resource under /listings/:id/tender/.
	api.POST("/listings/:id/tender/milestones", middleware.RequireTier(2), tenderH.CreateMilestone)
	api.GET("/listings/:id/tender/milestones", middleware.RequireTier(2), tenderH.ListMilestones)
	// /milestones/progress MUST be registered before /milestones/:milestoneId so the static segment
	// takes precedence over the param route.
	api.GET("/listings/:id/tender/milestones/progress", middleware.RequireTier(2), tenderH.GetMilestoneProgress)
	api.PATCH("/listings/:id/tender/milestones/:milestoneId", middleware.RequireTier(2), tenderH.UpdateMilestone)
	// Collaborators: apply is vendor-initiated; accept/reject are owner-initiated; exit is vendor-initiated.
	api.POST("/tender/roles/:roleId/apply", middleware.RequireTier(2), tenderH.ApplyToRole)
	api.GET("/listings/:id/tender/collaborators", middleware.RequireTier(2), tenderH.ListCollaborators)
	api.POST("/tender/collaborators/:id/accept", middleware.RequireTier(2), tenderH.AcceptCollaborator)
	api.POST("/tender/collaborators/:id/reject", middleware.RequireTier(2), tenderH.RejectCollaborator)
	api.POST("/tender/collaborators/:id/exit", middleware.RequireTier(2), tenderH.ExitCollaborator)

	// Vendor profile — own-profile only (Slice-1). Tier>=2 for both read and write.
	api.PUT("/vendor-profile", middleware.RequireTier(2), vendorProfileH.Upsert)
	api.GET("/vendor-profile", middleware.RequireTier(2), vendorProfileH.GetOwn)

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
