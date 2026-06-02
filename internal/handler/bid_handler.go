package handler

import (
	"net/http"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/platform/httpx"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// BidHandler handles bid-related endpoints.
type BidHandler struct {
	svc *service.BidService
}

// NewBidHandler returns a BidHandler.
func NewBidHandler(svc *service.BidService) *BidHandler {
	return &BidHandler{svc: svc}
}

// CreateBidRequest is the POST /v1/listings/:id/bids request body.
type CreateBidRequest struct {
	Amount   string `json:"amount"` // numeric as string to preserve precision
	Currency string `json:"currency"`
	Message  string `json:"message"`
}

// CreateBid handles POST /v1/listings/:id/bids.
// bidder_user_id is set from X-User-Id — NEVER from the request body.
func (h *BidHandler) CreateBid(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid listing id")
		return
	}

	var req CreateBidRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "amount must be a valid decimal")
		return
	}

	currency := req.Currency
	if currency == "" {
		currency = "TWD"
	}

	bid, err := h.svc.CreateBid(c.Request.Context(), &service.CreateBidInput{
		ListingID:    listingID,
		BidderUserID: identity.UserID, // from header only
		Amount:       amount,
		Currency:     currency,
		Message:      req.Message,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, bid)
}

// ListBidsForListing handles GET /v1/listings/:id/bids.
// Owner-only: X-User-Id must equal listing.owner_user_id.
func (h *BidHandler) ListBidsForListing(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	listingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid listing id")
		return
	}

	bids, err := h.svc.ListBidsForListing(c.Request.Context(), listingID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if bids == nil {
		bids = []*domain.Bid{}
	}

	httpx.OK(c, bids)
}

// ListMyBids handles GET /v1/bids — self-scoped bid list.
func (h *BidHandler) ListMyBids(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	bids, err := h.svc.ListMyBids(c.Request.Context(), identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if bids == nil {
		bids = []*domain.Bid{}
	}

	httpx.OK(c, bids)
}

// AcceptBid handles POST /v1/bids/:id/accept.
// IDOR: X-User-Id must equal owning listing.owner_user_id.
func (h *BidHandler) AcceptBid(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	bidID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid bid id")
		return
	}

	award, err := h.svc.AcceptBid(c.Request.Context(), bidID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, award)
}

// RejectBid handles POST /v1/bids/:id/reject.
// IDOR: X-User-Id must equal owning listing.owner_user_id.
func (h *BidHandler) RejectBid(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	bidID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid bid id")
		return
	}

	bid, err := h.svc.RejectBid(c.Request.Context(), bidID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, bid)
}

// WithdrawBid handles POST /v1/bids/:id/withdraw.
// IDOR: X-User-Id must equal bid.bidder_user_id.
func (h *BidHandler) WithdrawBid(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	bidID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid bid id")
		return
	}

	bid, err := h.svc.WithdrawBid(c.Request.Context(), bidID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, bid)
}
