package handler

import (
	"net/http"
	"strconv"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/platform/httpx"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const maxBodyBytes = 1 << 20 // 1 MB

// ListingHandler handles listing CRUD endpoints.
type ListingHandler struct {
	svc *service.ListingService
}

// NewListingHandler returns a ListingHandler.
func NewListingHandler(svc *service.ListingService) *ListingHandler {
	return &ListingHandler{svc: svc}
}

// CreateListingRequest is the POST /v1/listings request body.
type CreateListingRequest struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	BudgetMin   *string `json:"budgetMin"` // numeric as string to preserve precision
	BudgetMax   *string `json:"budgetMax"`
	Currency    string  `json:"currency"`
}

// Create handles POST /v1/listings.
// Requires valid identity (RequireValidIdentity) + Tier>=2.
// owner_user_id is set from X-User-Id — NEVER from the request body.
func (h *ListingHandler) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	var req CreateListingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := &service.CreateListingInput{
		OwnerUserID: identity.UserID, // from header only
		Title:       req.Title,
		Description: req.Description,
		Currency:    req.Currency,
	}

	if req.BudgetMin != nil {
		d, err := decimal.NewFromString(*req.BudgetMin)
		if err != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "budget_min must be a valid decimal")
			return
		}

		in.BudgetMin = &d
	}

	if req.BudgetMax != nil {
		d, err := decimal.NewFromString(*req.BudgetMax)
		if err != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "budget_max must be a valid decimal")
			return
		}

		in.BudgetMax = &d
	}

	if in.Currency == "" {
		in.Currency = "TWD"
	}

	listing, err := h.svc.CreateListing(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, listing)
}

// List handles GET /v1/listings.
// Supports optional ?mine=true and ?status=OPEN|AWARDED|CLOSED.
func (h *ListingHandler) List(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	// VisibleToUserID enforces the listing visibility rule in SQL (P0 IDOR fix):
	// OPEN listings are public; non-OPEN (AWARDED/CLOSED) are restricted to their
	// owner. Without this, ?status=AWARDED|CLOSED enumerated every user's rows.
	filter := store.ListingFilter{Limit: 20, VisibleToUserID: identity.UserID}

	if mine := c.Query("mine"); mine == "true" {
		filter.OwnerUserID = &identity.UserID
	}

	if statusStr := c.Query("status"); statusStr != "" {
		switch domain.ListingStatus(statusStr) {
		case domain.ListingStatusOpen, domain.ListingStatusAwarded, domain.ListingStatusClosed:
			s := domain.ListingStatus(statusStr)
			filter.Status = &s
		default:
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid status filter")
			return
		}
	} else {
		// Default: show OPEN listings.
		open := domain.ListingStatusOpen
		filter.Status = &open
	}

	listings, err := h.svc.ListListings(c.Request.Context(), filter)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if listings == nil {
		listings = []*domain.Listing{}
	}

	httpx.OK(c, listings)
}

// Search handles GET /v1/listings/search.
//
// Query params: q (full-text), status, minBudget, maxBudget, cursor, limit.
// Results are keyset-paginated (newest-first). Non-OPEN listings are only
// visible to their owner — enforced in the service layer.
func (h *ListingHandler) Search(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	in := &service.SearchListingsInput{
		CallerID: identity.UserID,
		Query:    c.Query("q"),
		Cursor:   c.Query("cursor"),
	}

	if statusStr := c.Query("status"); statusStr != "" {
		s := domain.ListingStatus(statusStr)
		in.Status = &s
	}

	if minStr := c.Query("minBudget"); minStr != "" {
		d, err := decimal.NewFromString(minStr)
		if err != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "minBudget must be a valid decimal")
			return
		}

		in.BudgetMin = &d
	}

	if maxStr := c.Query("maxBudget"); maxStr != "" {
		d, err := decimal.NewFromString(maxStr)
		if err != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "maxBudget must be a valid decimal")
			return
		}

		in.BudgetMax = &d
	}

	if limitStr := c.Query("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit < 0 {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "limit must be a non-negative integer")
			return
		}

		in.Limit = limit
	}

	result, err := h.svc.SearchListings(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, result)
}

// GetByID handles GET /v1/listings/:id.
// Applies the listing visibility rule (P0 IDOR fix): OPEN listings are visible
// to any authenticated caller, non-OPEN listings only to their owner; otherwise
// 404 (never 403, to avoid resource enumeration).
func (h *ListingHandler) GetByID(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid listing id")
		return
	}

	listing, err := h.svc.GetListing(c.Request.Context(), id, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, listing)
}

// UpdateListingRequest is the PATCH /v1/listings/:id request body.
type UpdateListingRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	BudgetMin   *string `json:"budgetMin"`
	BudgetMax   *string `json:"budgetMax"`
	Currency    *string `json:"currency"`
}

// Update handles PATCH /v1/listings/:id.
// Owner-only: X-User-Id must equal listing.owner_user_id.
func (h *ListingHandler) Update(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid listing id")
		return
	}

	var req UpdateListingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := service.UpdateListingInput{
		ID:          id,
		CallerID:    identity.UserID,
		Title:       req.Title,
		Description: req.Description,
		Currency:    req.Currency,
	}

	if req.BudgetMin != nil {
		d, parseErr := decimal.NewFromString(*req.BudgetMin)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "budget_min must be a valid decimal")
			return
		}

		in.BudgetMin = &d
	}

	if req.BudgetMax != nil {
		d, parseErr := decimal.NewFromString(*req.BudgetMax)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "budget_max must be a valid decimal")
			return
		}

		in.BudgetMax = &d
	}

	listing, err := h.svc.UpdateListing(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, listing)
}
