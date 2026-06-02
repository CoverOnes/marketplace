package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/gin-gonic/gin"
)

// ErrorResponse is the machine-readable error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the stable code, human message, and optional details.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Err sends a structured error response, translating domain errors to HTTP codes.
func Err(c *gin.Context, err error) {
	code, status, message, details := translate(err)
	c.JSON(status, ErrorResponse{Error: ErrorBody{
		Code:    code,
		Message: message,
		Details: details,
	}})
}

// ErrCode sends a raw code/status/message triple (for handler-generated errors
// that don't map cleanly to domain sentinels).
func ErrCode(c *gin.Context, status int, code, message string, details ...any) {
	var d any
	if len(details) > 0 {
		d = details[0]
	}

	c.JSON(status, ErrorResponse{Error: ErrorBody{
		Code:    code,
		Message: message,
		Details: d,
	}})
}

// translate maps domain / sentinel errors to HTTP status + machine code.
//
//nolint:unparam // details: reserved for structured error payload; always nil today but part of the stable contract
func translate(err error) (code string, status int, message string, details any) {
	switch {
	case errors.Is(err, domain.ErrListingNotFound), errors.Is(err, domain.ErrNotFound):
		return "LISTING_NOT_FOUND", http.StatusNotFound, "listing not found", nil

	case errors.Is(err, domain.ErrBidNotFound):
		return "BID_NOT_FOUND", http.StatusNotFound, "bid not found", nil

	case errors.Is(err, domain.ErrAwardNotFound):
		return "AWARD_NOT_FOUND", http.StatusNotFound, "award not found", nil

	case errors.Is(err, domain.ErrBidNotPending):
		return "BID_NOT_PENDING", http.StatusConflict, "bid is not in pending state", nil

	case errors.Is(err, domain.ErrBidAlreadyExists):
		return "BID_ALREADY_EXISTS", http.StatusConflict, "a pending bid already exists for this listing", nil

	case errors.Is(err, domain.ErrBidOnOwnListing):
		return "BID_ON_OWN_LISTING", http.StatusUnprocessableEntity, "cannot bid on your own listing", nil

	case errors.Is(err, domain.ErrListingNotOpen):
		return "LISTING_NOT_OPEN", http.StatusConflict, "listing is not open for bids", nil

	case errors.Is(err, domain.ErrConflict):
		return "CONFLICT", http.StatusConflict, "conflict detected", nil

	case errors.Is(err, domain.ErrForbidden):
		return "FORBIDDEN", http.StatusForbidden, "forbidden", nil

	case errors.Is(err, domain.ErrUnauthorized):
		return "UNAUTHORIZED", http.StatusUnauthorized, "unauthorized", nil

	case errors.Is(err, domain.ErrKYCTierRequired):
		return "KYC_TIER_REQUIRED", http.StatusForbidden, "kyc verification required", nil

	case errors.Is(err, domain.ErrValidation):
		return "VALIDATION_ERROR", http.StatusBadRequest, err.Error(), nil

	default:
		slog.Error("unhandled internal error", "err", err)
		return "INTERNAL_ERROR", http.StatusInternalServerError, "internal server error", nil
	}
}
