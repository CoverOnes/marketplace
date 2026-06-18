package handler

import (
	"net/http"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/platform/httpx"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TenderHandler handles tender role, collaborator, and milestone endpoints.
type TenderHandler struct {
	svc *service.TenderService
}

// NewTenderHandler returns a TenderHandler.
func NewTenderHandler(svc *service.TenderService) *TenderHandler {
	return &TenderHandler{svc: svc}
}

// --- Role endpoints ---

// CreateRoleRequest is the POST /v1/listings/:id/tender/roles request body.
type CreateRoleRequest struct {
	Title            string `json:"title"`
	Description      string `json:"description"`
	MaxCollaborators *int   `json:"maxCollaborators"`
	ProfitShareBPS   *int   `json:"profitShareBps"`
	ProfitShareNote  string `json:"profitShareNote"`
	SortOrder        int    `json:"sortOrder"`
}

// CreateRole handles POST /v1/listings/:id/tender/roles.
// Owner-only.
func (h *TenderHandler) CreateRole(c *gin.Context) {
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

	var req CreateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	role, err := h.svc.CreateRole(c.Request.Context(), &service.CreateRoleInput{
		ListingID:        listingID,
		CallerID:         identity.UserID,
		Title:            req.Title,
		Description:      req.Description,
		MaxCollaborators: req.MaxCollaborators,
		ProfitShareBPS:   req.ProfitShareBPS,
		ProfitShareNote:  req.ProfitShareNote,
		SortOrder:        req.SortOrder,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, role)
}

// ListRoles handles GET /v1/listings/:id/tender/roles.
// Owner-only.
func (h *TenderHandler) ListRoles(c *gin.Context) {
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

	roles, err := h.svc.ListRoles(c.Request.Context(), listingID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if roles == nil {
		roles = []*domain.TenderRole{}
	}

	httpx.OK(c, roles)
}

// CloseRole handles POST /v1/listings/:id/tender/roles/:roleId/close.
// Owner-only.
func (h *TenderHandler) CloseRole(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	roleID, err := uuid.Parse(c.Param("roleId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid role id")
		return
	}

	role, err := h.svc.CloseRole(c.Request.Context(), &service.CloseRoleInput{
		RoleID:   roleID,
		CallerID: identity.UserID,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, role)
}

// --- Collaborator endpoints ---

// ApplyToRoleRequest is the POST /v1/tender/roles/:roleId/apply request body.
type ApplyToRoleRequest struct {
	JoinMessage string `json:"joinMessage"`
}

// ApplyToRole handles POST /v1/tender/roles/:roleId/apply.
// Vendor self-action.
func (h *TenderHandler) ApplyToRole(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	roleID, err := uuid.Parse(c.Param("roleId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid role id")
		return
	}

	var req ApplyToRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	collab, err := h.svc.ApplyToRole(c.Request.Context(), &service.ApplyToRoleInput{
		RoleID:       roleID,
		VendorUserID: identity.UserID,
		KYCTier:      identity.KYCTier,
		JoinMessage:  req.JoinMessage,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, collab)
}

// AcceptCollaborator handles POST /v1/tender/collaborators/:id/accept.
// Owner-only.
func (h *TenderHandler) AcceptCollaborator(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	collaboratorID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid collaborator id")
		return
	}

	collab, err := h.svc.AcceptCollaborator(c.Request.Context(), &service.AcceptCollaboratorInput{
		CollaboratorID: collaboratorID,
		CallerID:       identity.UserID,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, collab)
}

// RejectCollaborator handles POST /v1/tender/collaborators/:id/reject.
// Owner-only.
func (h *TenderHandler) RejectCollaborator(c *gin.Context) {
	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	collaboratorID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid collaborator id")
		return
	}

	collab, err := h.svc.RejectCollaborator(c.Request.Context(), &service.RejectCollaboratorInput{
		CollaboratorID: collaboratorID,
		CallerID:       identity.UserID,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, collab)
}

// ExitCollaboratorRequest is the POST /v1/tender/collaborators/:id/exit request body.
type ExitCollaboratorRequest struct {
	Reason string `json:"reason"`
}

// ExitCollaborator handles POST /v1/tender/collaborators/:id/exit.
// Vendor self-action.
func (h *TenderHandler) ExitCollaborator(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

	identity, ok := middleware.IdentityFromCtx(c)
	if !ok {
		httpx.ErrCode(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	collaboratorID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid collaborator id")
		return
	}

	var req ExitCollaboratorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	collab, err := h.svc.ExitCollaborator(c.Request.Context(), &service.ExitCollaboratorInput{
		CollaboratorID: collaboratorID,
		CallerID:       identity.UserID,
		Reason:         req.Reason,
	})
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.OK(c, collab)
}

// ListCollaborators handles GET /v1/listings/:id/tender/collaborators.
// Owner-only.
func (h *TenderHandler) ListCollaborators(c *gin.Context) {
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

	collabs, err := h.svc.ListCollaborators(c.Request.Context(), listingID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if collabs == nil {
		collabs = []*domain.TenderCollaborator{}
	}

	httpx.OK(c, collabs)
}

// --- Milestone endpoints ---

// CreateMilestoneRequest is the POST /v1/listings/:id/tender/milestones request body.
type CreateMilestoneRequest struct {
	Title    string  `json:"title"`
	DueDate  *string `json:"dueDate"`  // RFC3339 date string, nil = no due date
	Amount   *string `json:"amount"`   // decimal string, nil = no fixed amount
	Currency *string `json:"currency"` // 3-letter code, must be set when amount is set
}

// CreateMilestone handles POST /v1/listings/:id/tender/milestones.
// Owner-only.
func (h *TenderHandler) CreateMilestone(c *gin.Context) {
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

	var req CreateMilestoneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	in := &service.CreateMilestoneInput{
		ListingID: listingID,
		CallerID:  identity.UserID,
		Title:     req.Title,
		Amount:    req.Amount,
		Currency:  req.Currency,
	}

	if req.DueDate != nil {
		t, parseErr := time.Parse(time.RFC3339, *req.DueDate)
		if parseErr != nil {
			httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "due_date must be RFC3339 format")
			return
		}

		in.DueDate = &t
	}

	milestone, err := h.svc.CreateMilestone(c.Request.Context(), in)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	httpx.Created(c, milestone)
}

// ListMilestones handles GET /v1/listings/:id/tender/milestones.
// Owner-only.
func (h *TenderHandler) ListMilestones(c *gin.Context) {
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

	milestones, err := h.svc.ListMilestones(c.Request.Context(), listingID, identity.UserID)
	if err != nil {
		httpx.Err(c, err)
		return
	}

	if milestones == nil {
		milestones = []*domain.TenderMilestone{}
	}

	httpx.OK(c, milestones)
}
