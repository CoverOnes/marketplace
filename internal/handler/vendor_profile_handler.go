package handler

import (
	"errors"
	"net/http"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/platform/httpx"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
)

// VendorProfileHandler handles vendor profile endpoints.
type VendorProfileHandler struct {
	svc *service.VendorProfileService
}

// NewVendorProfileHandler returns a VendorProfileHandler.
func NewVendorProfileHandler(svc *service.VendorProfileService) *VendorProfileHandler {
	return &VendorProfileHandler{svc: svc}
}

// UpsertVendorProfileRequest is the PUT /v1/vendor-profile request body.
// owner_user_id is NEVER read from the body — it comes exclusively from the
// gateway-injected identity (middleware.IdentityFromCtx).
type UpsertVendorProfileRequest struct {
	DisplayName string   `json:"displayName"`
	Headline    *string  `json:"headline"`
	Bio         *string  `json:"bio"`
	Skills      []string `json:"skills"`
}

// Upsert handles PUT /v1/vendor-profile.
// Creates or updates the calling user's vendor profile (idempotent).
// Returns 200 on both create and update (upsert semantics).
// Requires RequireTier(2).
func (h *VendorProfileHandler) Upsert(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	var req UpsertVendorProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := service.UpsertVendorProfileInput{
		OwnerUserID: identity.UserID, // exclusively from identity — never from body
		DisplayName: req.DisplayName,
		Headline:    req.Headline,
		Bio:         req.Bio,
		Skills:      req.Skills,
	}

	profile, err := h.svc.Upsert(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, profile)
}

// GetOwn handles GET /v1/vendor-profile.
// Returns the calling user's vendor profile, or 404 when none exists.
// Requires RequireTier(2).
func (h *VendorProfileHandler) GetOwn(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	profile, err := h.svc.Get(c.Request.Context(), identity.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.ErrCode(c, http.StatusNotFound, "VENDOR_PROFILE_NOT_FOUND", "vendor profile not found")
			return
		}

		httpx.Err(c, err)

		return
	}

	httpx.OK(c, profile)
}
