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
// Split into classic and tender sub-functions to stay under gocyclo limit (15).
//
//nolint:unparam // details: always nil today; reserved for structured error payloads in future extensions
func translate(err error) (code string, status int, message string, details any) {
	if code, status, message, ok := translateClassic(err); ok {
		return code, status, message, nil
	}

	if code, status, message, ok := translateTender(err); ok {
		return code, status, message, nil
	}

	slog.Error("unhandled internal error", "err", err)

	return "INTERNAL_ERROR", http.StatusInternalServerError, "internal server error", nil
}

// translateClassic handles errors from the CLASSIC 1:1 listing/bid/award flow.
func translateClassic(err error) (code string, status int, message string, ok bool) {
	switch {
	case errors.Is(err, domain.ErrListingNotFound), errors.Is(err, domain.ErrNotFound):
		return "LISTING_NOT_FOUND", http.StatusNotFound, "listing not found", true

	case errors.Is(err, domain.ErrBidNotFound):
		return "BID_NOT_FOUND", http.StatusNotFound, "bid not found", true

	case errors.Is(err, domain.ErrAwardNotFound):
		return "AWARD_NOT_FOUND", http.StatusNotFound, "award not found", true

	case errors.Is(err, domain.ErrBidNotPending):
		return "BID_NOT_PENDING", http.StatusConflict, "bid is not in pending state", true

	case errors.Is(err, domain.ErrBidAlreadyExists):
		return "BID_ALREADY_EXISTS", http.StatusConflict, "a pending bid already exists for this listing", true

	case errors.Is(err, domain.ErrBidOnOwnListing):
		return "BID_ON_OWN_LISTING", http.StatusUnprocessableEntity, "cannot bid on your own listing", true

	case errors.Is(err, domain.ErrListingNotOpen):
		return "LISTING_NOT_OPEN", http.StatusConflict, "listing is not open for bids", true

	case errors.Is(err, domain.ErrConflict):
		return "CONFLICT", http.StatusConflict, "conflict detected", true

	case errors.Is(err, domain.ErrForbidden):
		return "FORBIDDEN", http.StatusForbidden, "forbidden", true

	case errors.Is(err, domain.ErrUnauthorized):
		return "UNAUTHORIZED", http.StatusUnauthorized, "unauthorized", true

	case errors.Is(err, domain.ErrKYCTierRequired):
		return "KYC_TIER_REQUIRED", http.StatusForbidden, "kyc verification required", true

	case errors.Is(err, domain.ErrValidation):
		return "VALIDATION_ERROR", http.StatusBadRequest, err.Error(), true
	}

	return "", 0, "", false
}

// translateTender handles errors from the tender (many-to-many) flow.
func translateTender(err error) (code string, status int, message string, ok bool) {
	switch {
	case errors.Is(err, domain.ErrTenderBidNotAllowed):
		return "TENDER_BID_NOT_ALLOWED", http.StatusConflict,
			"classic bid not allowed on a tender listing; use the tender collaborator API", true

	case errors.Is(err, domain.ErrTenderRoleNotFound):
		return "TENDER_ROLE_NOT_FOUND", http.StatusNotFound, "tender role not found", true

	case errors.Is(err, domain.ErrTenderCollaboratorNotFound):
		return "TENDER_COLLABORATOR_NOT_FOUND", http.StatusNotFound, "tender collaborator not found", true

	case errors.Is(err, domain.ErrTenderMilestoneNotFound):
		return "TENDER_MILESTONE_NOT_FOUND", http.StatusNotFound, "tender milestone not found", true

	case errors.Is(err, domain.ErrTenderRoleFilled):
		return "TENDER_ROLE_FILLED", http.StatusConflict, "tender role has reached maximum collaborators", true

	case errors.Is(err, domain.ErrTenderCollaboratorConflict):
		return "TENDER_COLLABORATOR_CONFLICT", http.StatusConflict,
			"a live application already exists for this role and vendor", true

	case errors.Is(err, domain.ErrInvalidTenderTransition):
		return "INVALID_TENDER_TRANSITION", http.StatusConflict, "invalid tender status transition", true

	case errors.Is(err, domain.ErrOpenRecruiterNotEnabled):
		return "OPEN_RECRUITER_NOT_ENABLED", http.StatusBadRequest,
			"open recruitment mode is not enabled until a later phase", true

	case errors.Is(err, domain.ErrNotTenderListing):
		return "NOT_TENDER_LISTING", http.StatusConflict, "listing is not a tender", true
	}

	return "", 0, "", false
}
