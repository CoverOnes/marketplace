package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/handler"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub attachment store for handler tests ---

type stubAttachStoreH struct {
	attachments map[uuid.UUID]*domain.ListingAttachment
}

func newStubAttachStoreH() *stubAttachStoreH {
	return &stubAttachStoreH{attachments: make(map[uuid.UUID]*domain.ListingAttachment)}
}

func (s *stubAttachStoreH) Create(_ context.Context, a *domain.ListingAttachment) error {
	s.attachments[a.ID] = a
	return nil
}

func (s *stubAttachStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.ListingAttachment, error) {
	a, ok := s.attachments[id]
	if !ok {
		return nil, domain.ErrAttachmentNotFound
	}

	return a, nil
}

func (s *stubAttachStoreH) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.ListingAttachment, error) {
	var out []*domain.ListingAttachment

	for _, a := range s.attachments {
		if a.ListingID == listingID && a.DetachedAt == nil {
			out = append(out, a)
		}
	}

	return out, nil
}

func (s *stubAttachStoreH) CountActiveByListing(_ context.Context, listingID uuid.UUID) (int, error) {
	count := 0

	for _, a := range s.attachments {
		if a.ListingID == listingID && a.DetachedAt == nil {
			count++
		}
	}

	return count, nil
}

func (s *stubAttachStoreH) Detach(_ context.Context, id, _ uuid.UUID) error {
	a, ok := s.attachments[id]
	if !ok || a.DetachedAt != nil {
		return domain.ErrAttachmentNotFound
	}

	now := time.Now().UTC()
	a.DetachedAt = &now

	return nil
}

// --- stub collaborator store for handler tests ---

type stubCollabStoreH struct{}

func (s *stubCollabStoreH) ListByListing(_ context.Context, _ uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return nil, nil
}

func (s *stubCollabStoreH) Create(_ context.Context, _ *domain.TenderCollaborator) error {
	return nil
}

func (s *stubCollabStoreH) GetByID(_ context.Context, _ uuid.UUID) (*domain.TenderCollaborator, error) {
	return nil, nil
}

func (s *stubCollabStoreH) GetByIDForUpdate(_ context.Context, _ uuid.UUID) (*domain.TenderCollaborator, error) {
	return nil, nil
}

func (s *stubCollabStoreH) CountApprovedByRole(_ context.Context, _ uuid.UUID) (int, error) {
	return 0, nil
}

func (s *stubCollabStoreH) ListByRole(_ context.Context, _ uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return nil, nil
}

func (s *stubCollabStoreH) Update(_ context.Context, _ *domain.TenderCollaborator) error {
	return nil
}

// --- compile-time interface satisfaction checks ---

var (
	_ store.ListingAttachmentStore  = (*stubAttachStoreH)(nil)
	_ store.TenderCollaboratorStore = (*stubCollabStoreH)(nil)
)

// buildAttachmentRouter builds a test router with AttachmentHandler wired through
// the real service backed by in-memory stub stores. No Docker / testcontainers needed.
// fileClient is nil — service.Attach skips S2S registration when fileClient is nil
// (dev mode); config validation prevents nil fileClient in non-dev environments.
func buildAttachmentRouter(listingStore store.ListingStore, attachStore store.ListingAttachmentStore) *gin.Engine {
	gin.SetMode(gin.TestMode)

	svc := service.NewAttachmentService(attachStore, listingStore, &stubCollabStoreH{}, nil)
	h := handler.NewAttachmentHandler(svc)

	r := gin.New()

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())
	api.POST("/listings/:id/attachments", middleware.RequireTier(2), h.Attach)
	api.GET("/listings/:id/attachments", middleware.RequireTier(1), h.List)
	api.GET("/listings/:id/attachments/:attachmentId/download-url", middleware.RequireTier(1), h.DownloadURL)
	api.DELETE("/listings/:id/attachments/:attachmentId", middleware.RequireTier(2), h.Detach)

	return r
}

// validAttachBody returns a request body map for a valid Attach request.
func validAttachBody() map[string]any {
	return map[string]any{
		"fileId":      uuid.New().String(),
		"filename":    "report.pdf",
		"contentType": "application/pdf",
		"sizeBytes":   int64(1024),
	}
}

// TestAttachmentHandler_Attach_FilenameControlChars verifies that POST
// /v1/listings/:id/attachments returns HTTP 400 with the exact message
// "filename contains invalid characters" when the filename contains ASCII
// control characters (null bytes, CRLF, or other chars below 0x20 except tab).
//
// This test FAILS if filenameContainsControlChars is reverted from attachment_handler.go:
// without the guard the handler would pass the bad filename to the service layer, which
// does not validate filenames, and the row would be persisted with a control-char filename.
func TestAttachmentHandler_Attach_FilenameControlChars(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listingID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Test listing",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name     string
		filename string
	}{
		{
			// Null byte (\x00) — must be rejected; breaks C-string semantics in
			// downstream systems and can truncate Content-Disposition headers.
			name:     "null byte in filename",
			filename: "bad\x00file.pdf",
		},
		{
			// CRLF injection: \r\n followed by a header lookalike. If stored verbatim
			// and placed into a Content-Disposition header, the injected line becomes
			// an HTTP response header visible to the client.
			name:     "CRLF header injection in filename",
			filename: "a\r\nX-Injected: y",
		},
		{
			// SOH (0x01) is a generic control char below 0x20 not exempted by the guard.
			name:     "SOH control character in filename",
			filename: "file\x01name.pdf",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH(openListing)
			as := newStubAttachStoreH()
			r := buildAttachmentRouter(ls, as)

			bodyMap := validAttachBody()
			bodyMap["filename"] = tc.filename

			bodyBytes, _ := json.Marshal(bodyMap)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/v1/listings/"+listingID.String()+"/attachments", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", ownerID.String())
			req.Header.Set("X-Kyc-Tier", "2")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

			var resp map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

			errBody, ok := resp["error"].(map[string]any)
			require.True(t, ok, "expected error envelope in body: %s", w.Body.String())
			assert.Equal(t, "VALIDATION_ERROR", errBody["code"])
			assert.Equal(t, "filename contains invalid characters", errBody["message"])
		})
	}
}

// TestAttachmentHandler_Attach_MalformedJSON verifies that POST
// /v1/listings/:id/attachments returns HTTP 400 with the generic message
// "request body is invalid" when the JSON body is malformed or type-mismatched.
//
// This test FAILS if the info-leak guard is reverted from attachment_handler.go:
// without the guard the raw JSON parser error message (e.g. "cannot unmarshal string
// into Go struct field AttachRequest.sizeBytes of type int64") would be forwarded
// to the caller, leaking internal type information.
func TestAttachmentHandler_Attach_MalformedJSON(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listingID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Test listing",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name    string
		rawBody string
	}{
		{
			// Truncated at a field value — JSON parser errors here with an unexpected-EOF.
			name:    "truncated JSON object",
			rawBody: `{"filename":`,
		},
		{
			// Wrong type for sizeBytes (string instead of int64) — type-mismatch error.
			name:    "wrong type for sizeBytes",
			rawBody: `{"fileId":"` + uuid.New().String() + `","filename":"f.pdf","contentType":"application/pdf","sizeBytes":"not-an-int"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH(openListing)
			as := newStubAttachStoreH()
			r := buildAttachmentRouter(ls, as)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/v1/listings/"+listingID.String()+"/attachments",
				bytes.NewBufferString(tc.rawBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", ownerID.String())
			req.Header.Set("X-Kyc-Tier", "2")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

			var resp map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

			errBody, ok := resp["error"].(map[string]any)
			require.True(t, ok, "expected error envelope in body: %s", w.Body.String())
			assert.Equal(t, "VALIDATION_ERROR", errBody["code"])
			// Must be the generic message — NOT parser internals like "cannot unmarshal...".
			assert.Equal(t, "request body is invalid", errBody["message"],
				"handler must not forward raw parser error to caller (info-leak guard)")
		})
	}
}

// TestAttachmentHandler_Attach_HappyPath is the control case: a clean, valid POST
// /v1/listings/:id/attachments reaches the service and returns 201. This proves
// that the 400 rejections in the tests above are specific to bad inputs, not
// blanket failures of the route.
func TestAttachmentHandler_Attach_HappyPath(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listingID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		Currency:    "TWD",
		Title:       "Test listing",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	ls := newStubListingStoreH(openListing)
	as := newStubAttachStoreH()
	r := buildAttachmentRouter(ls, as)

	bodyBytes, _ := json.Marshal(validAttachBody())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/v1/listings/"+listingID.String()+"/attachments", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", ownerID.String())
	req.Header.Set("X-Kyc-Tier", "2")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code, "valid request must reach service and return 201; body: %s", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	data, ok := resp["data"].(map[string]any)
	require.True(t, ok, "expected data envelope in body: %s", w.Body.String())
	assert.NotEmpty(t, data["id"], "attachment id must be set")
	assert.Equal(t, listingID.String(), data["listingId"])
}
