package handler

import (
	"errors"
	"net/http"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/platform/httpx"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Client-asserted metadata bounds. These mirror the DB CHECK constraints in
// migration 000011 so an over-long value is rejected with a clean 400 at the
// handler instead of surfacing a raw DB constraint violation as a 500
// (backend-security-design §5.2: validate client-side, don't delegate to DB CHECK).
const (
	maxFilenameLen    = 255
	maxContentTypeLen = 127
	// maxAttachmentSizeBytes caps the client-asserted display size at 5 GiB — a
	// sane upper bound that rejects absurd values (e.g. math.MaxInt64) while
	// comfortably exceeding any real document/image attachment.
	maxAttachmentSizeBytes = 5 * 1024 * 1024 * 1024
)

// AttachmentHandler handles listing attachment endpoints.
type AttachmentHandler struct {
	svc *service.AttachmentService
}

// NewAttachmentHandler returns an AttachmentHandler.
func NewAttachmentHandler(svc *service.AttachmentService) *AttachmentHandler {
	return &AttachmentHandler{svc: svc}
}

// AttachRequest is the POST /v1/listings/:id/attachments request body.
// Metadata (Filename, ContentType, SizeBytes) is client-asserted display metadata
// that is copied at attach time — the file service holds the authoritative file record.
type AttachRequest struct {
	FileID      string `json:"fileId"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// attachResponse is the response body for a successful attachment creation.
type attachResponse struct {
	ID          string `json:"id"`
	ListingID   string `json:"listingId"`
	FileID      string `json:"fileId"`
	UploaderID  string `json:"uploaderId"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	CreatedAt   string `json:"createdAt"`
}

// downloadURLResponse is the response body for a download URL request.
type downloadURLResponse struct {
	URL string `json:"url"`
}

// Attach handles POST /v1/listings/:id/attachments.
// Caller must be the listing owner or an APPROVED tender collaborator.
func (h *AttachmentHandler) Attach(c *gin.Context) {
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

	var req AttachRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	if req.FileID == "" {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "fileId is required")
		return
	}

	fileID, err := uuid.Parse(req.FileID)
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "fileId must be a valid UUID")
		return
	}

	if req.Filename == "" {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "filename is required")
		return
	}

	if len(req.Filename) > maxFilenameLen {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "filename must be at most 255 characters")
		return
	}

	if req.ContentType == "" {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "contentType is required")
		return
	}

	if len(req.ContentType) > maxContentTypeLen {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "contentType must be at most 127 characters")
		return
	}

	if req.SizeBytes <= 0 {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "sizeBytes must be > 0")
		return
	}

	if req.SizeBytes > maxAttachmentSizeBytes {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "sizeBytes exceeds the maximum allowed")
		return
	}

	a, svcErr := h.svc.Attach(c.Request.Context(), listingID, identity.UserID, service.AttachInput{
		FileID:      fileID,
		Filename:    req.Filename,
		ContentType: req.ContentType,
		SizeBytes:   req.SizeBytes,
	})
	if svcErr != nil {
		translateAttachmentError(c, svcErr)
		return
	}

	httpx.Created(c, attachResponse{
		ID:          a.ID.String(),
		ListingID:   a.ListingID.String(),
		FileID:      a.FileID.String(),
		UploaderID:  a.UploaderID.String(),
		Filename:    a.Filename,
		ContentType: a.ContentType,
		SizeBytes:   a.SizeBytes,
		CreatedAt:   a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
}

// List handles GET /v1/listings/:id/attachments.
// OPEN listings are publicly readable; others require owner or APPROVED collaborator.
func (h *AttachmentHandler) List(c *gin.Context) {
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

	attachments, svcErr := h.svc.List(c.Request.Context(), listingID, identity.UserID)
	if svcErr != nil {
		translateAttachmentError(c, svcErr)
		return
	}

	// Return an empty array (not null) when there are no attachments.
	result := make([]attachResponse, 0, len(attachments))

	for _, a := range attachments {
		result = append(result, attachResponse{
			ID:          a.ID.String(),
			ListingID:   a.ListingID.String(),
			FileID:      a.FileID.String(),
			UploaderID:  a.UploaderID.String(),
			Filename:    a.Filename,
			ContentType: a.ContentType,
			SizeBytes:   a.SizeBytes,
			CreatedAt:   a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	httpx.OK(c, result)
}

// DownloadURL handles GET /v1/listings/:id/attachments/:attachmentId/download-url.
// Returns a presigned download URL via S2S call to the file service.
func (h *AttachmentHandler) DownloadURL(c *gin.Context) {
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

	attachmentID, err := uuid.Parse(c.Param("attachmentId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid attachment id")
		return
	}

	url, svcErr := h.svc.DownloadURL(c.Request.Context(), listingID, attachmentID, identity.UserID)
	if svcErr != nil {
		translateAttachmentError(c, svcErr)
		return
	}

	httpx.OK(c, downloadURLResponse{URL: url})
}

// Detach handles DELETE /v1/listings/:id/attachments/:attachmentId.
// Caller must be the original uploader or the listing owner.
func (h *AttachmentHandler) Detach(c *gin.Context) {
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

	attachmentID, err := uuid.Parse(c.Param("attachmentId"))
	if err != nil {
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "invalid attachment id")
		return
	}

	if svcErr := h.svc.Detach(c.Request.Context(), listingID, attachmentID, identity.UserID); svcErr != nil {
		translateAttachmentError(c, svcErr)
		return
	}

	httpx.NoContent(c)
}

// translateAttachmentError maps attachment domain errors to HTTP responses.
// Raw downstream errors (ErrUpstreamFile) are always collapsed to a generic 502
// to prevent information leakage from the file service to API consumers.
func translateAttachmentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrListingNotFound):
		httpx.ErrCode(c, http.StatusNotFound, "LISTING_NOT_FOUND", "listing not found")

	case errors.Is(err, domain.ErrAttachmentNotFound):
		httpx.ErrCode(c, http.StatusNotFound, "ATTACHMENT_NOT_FOUND", "attachment not found")

	case errors.Is(err, domain.ErrAttachmentForbidden):
		httpx.ErrCode(c, http.StatusForbidden, "FORBIDDEN", "attachment operation forbidden")

	case errors.Is(err, domain.ErrAttachmentCapReached):
		httpx.ErrCode(c, http.StatusConflict, "ATTACHMENT_CAP_REACHED",
			"listing has reached the maximum of 10 attachments")

	case errors.Is(err, domain.ErrContentTypeNotAllowed):
		httpx.ErrCode(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"content type not allowed")

	case errors.Is(err, domain.ErrUpstreamFile):
		// Never leak raw file service error details.
		httpx.ErrCode(c, http.StatusBadGateway, "UPSTREAM_FILE_ERROR",
			"file service error; please retry")

	default:
		httpx.Err(c, err)
	}
}
