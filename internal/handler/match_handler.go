package handler

import (
	"net/http"
	"strconv"

	"github.com/CoverOnes/marketplace/internal/platform/httpx"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// MatchHandler handles the vendor-match endpoint.
type MatchHandler struct {
	svc *service.MatchService
}

// NewMatchHandler returns a MatchHandler.
func NewMatchHandler(svc *service.MatchService) *MatchHandler {
	return &MatchHandler{svc: svc}
}

// matchBreakdownResponse is the JSON shape for a score breakdown.
type matchBreakdownResponse struct {
	Skill       float64 `json:"skill"`
	Reliability float64 `json:"reliability"`
	Collab      float64 `json:"collab"`
	Fit         float64 `json:"fit"`
	Comm        float64 `json:"comm"`
}

// matchResultResponse is the JSON shape for a single ranked vendor.
type matchResultResponse struct {
	VendorUserID string                 `json:"vendorUserId"`
	OverallScore float64                `json:"overallScore"`
	Breakdown    matchBreakdownResponse `json:"breakdown"`
}

// matchResponseData is the JSON shape for the data envelope.
type matchResponseData struct {
	TenderID string                `json:"tenderId"`
	Partial  bool                  `json:"partial"`
	Results  []matchResultResponse `json:"results"`
}

// GetMatches handles GET /v1/tenders/:id/matches.
// Requires RequireValidIdentity + RequireTier(2).
// Query parameter: limit (default 10, cap 50).
func (h *MatchHandler) GetMatches(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")

		return
	}

	rawID := c.Param("id")

	tenderID, err := uuid.Parse(rawID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "tender id must be a valid UUID")

		return
	}

	// Parse limit query parameter.
	// Absent or empty → default 10. Present but non-integer → 400.
	limit := 10

	if rawLimit := c.Query("limit"); rawLimit != "" {
		parsed, parseErr := strconv.Atoi(rawLimit)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "limit must be an integer")

			return
		}

		limit = parsed
	}

	result, err := h.svc.GetMatches(c.Request.Context(), identity.UserID, tenderID, limit)
	if err != nil {
		httpx.Err(c, err)

		return
	}

	resp := matchResponseData{
		TenderID: result.TenderID.String(),
		Partial:  result.Partial,
		Results:  make([]matchResultResponse, 0, len(result.Results)),
	}

	for _, r := range result.Results {
		resp.Results = append(resp.Results, matchResultResponse{
			VendorUserID: r.VendorUserID.String(),
			OverallScore: r.OverallScore,
			Breakdown: matchBreakdownResponse{
				Skill:       r.Breakdown.Skill,
				Reliability: r.Breakdown.Reliability,
				Collab:      r.Breakdown.Collab,
				Fit:         r.Breakdown.Fit,
				Comm:        r.Breakdown.Comm,
			},
		})
	}

	httpx.OK(c, resp)
}
