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
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// --- stub listing store ---

type stubListingStoreH struct {
	listings map[uuid.UUID]*domain.Listing
}

func newStubListingStoreH(listings ...*domain.Listing) *stubListingStoreH {
	m := &stubListingStoreH{listings: make(map[uuid.UUID]*domain.Listing)}

	for _, l := range listings {
		m.listings[l.ID] = l
	}

	return m
}

func (s *stubListingStoreH) Create(_ context.Context, l *domain.Listing) error {
	s.listings[l.ID] = l
	return nil
}

func (s *stubListingStoreH) GetByID(_ context.Context, id uuid.UUID) (*domain.Listing, error) {
	l, ok := s.listings[id]
	if !ok {
		return nil, domain.ErrListingNotFound
	}

	return l, nil
}

// GetByIDForUpdate simulates SELECT ... FOR UPDATE; in-memory stubs need no locking.
func (s *stubListingStoreH) GetByIDForUpdate(_ context.Context, id uuid.UUID) (*domain.Listing, error) {
	l, ok := s.listings[id]
	if !ok {
		return nil, domain.ErrListingNotFound
	}

	return l, nil
}

func (s *stubListingStoreH) List(_ context.Context, _ store.ListingFilter) ([]*domain.Listing, error) {
	var result []*domain.Listing

	for _, l := range s.listings {
		result = append(result, l)
	}

	return result, nil
}

func (s *stubListingStoreH) Update(_ context.Context, l *domain.Listing) error {
	s.listings[l.ID] = l
	return nil
}

// buildListingRouter builds a router with ListingHandler wired.
func buildListingRouter(ls *stubListingStoreH) *gin.Engine {
	gin.SetMode(gin.TestMode)

	svc := service.NewListingService(ls)
	h := handler.NewListingHandler(svc)

	r := gin.New()

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())
	api.POST("/listings", middleware.RequireTier(2), h.Create)
	api.GET("/listings", middleware.RequireTier(1), h.List)
	api.GET("/listings/:id", middleware.RequireTier(1), h.GetByID)
	api.PATCH("/listings/:id", middleware.RequireTier(2), h.Update)

	return r
}

func TestListingHandler_Create(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name         string
		userIDHeader string
		tierHeader   string
		body         map[string]any
		wantStatus   int
		wantCode     string
	}{
		{
			name:         "happy path: create listing Tier2",
			userIDHeader: ownerID.String(),
			tierHeader:   "2",
			body: map[string]any{
				"title":    "Need a Go developer",
				"currency": "TWD",
			},
			wantStatus: http.StatusCreated,
		},
		{
			name:         "error: missing X-User-Id -> 401",
			userIDHeader: "",
			tierHeader:   "2",
			body: map[string]any{
				"title":    "Test",
				"currency": "TWD",
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
		},
		{
			name:         "error: tier 1 -> 403 KYC_TIER_REQUIRED",
			userIDHeader: ownerID.String(),
			tierHeader:   "1",
			body: map[string]any{
				"title":    "Test",
				"currency": "TWD",
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "KYC_TIER_REQUIRED",
		},
		{
			name:         "error: title empty -> validation",
			userIDHeader: ownerID.String(),
			tierHeader:   "2",
			body: map[string]any{
				"title":    "",
				"currency": "TWD",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:         "error: title contains null byte -> validation",
			userIDHeader: ownerID.String(),
			tierHeader:   "2",
			body: map[string]any{
				"title":    "bad\x00title",
				"currency": "TWD",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH()
			r := buildListingRouter(ls)

			body, _ := json.Marshal(tc.body)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/listings", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			if tc.userIDHeader != "" {
				req.Header.Set("X-User-Id", tc.userIDHeader)
			}

			if tc.tierHeader != "" {
				req.Header.Set("X-Kyc-Tier", tc.tierHeader)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}

func TestListingHandler_Update_IDOR(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	strangerID := uuid.New()
	listingID := uuid.New()

	openListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Title:       "Original Title",
		Description: "",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name       string
		callerID   uuid.UUID
		tierHeader string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "happy path: owner can update",
			callerID:   ownerID,
			tierHeader: "2",
			wantStatus: http.StatusOK,
		},
		{
			name:       "error: non-owner returns 404 (IDOR guard)",
			callerID:   strangerID,
			tierHeader: "2",
			wantStatus: http.StatusNotFound,
			wantCode:   "LISTING_NOT_FOUND",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH(openListing)
			r := buildListingRouter(ls)

			body, _ := json.Marshal(map[string]any{"title": "Updated Title"})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPatch, "/v1/listings/"+listingID.String(), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", tc.callerID.String())
			req.Header.Set("X-Kyc-Tier", tc.tierHeader)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}

func TestListingHandler_GetByID(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	listingID := uuid.New()
	existingListing := &domain.Listing{
		ID:          listingID,
		OwnerUserID: ownerID,
		Title:       "Existing",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		BudgetMin:   func() *decimal.Decimal { d := decimal.NewFromInt(100); return &d }(),
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name       string
		listingID  string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "happy path: existing listing",
			listingID:  listingID.String(),
			wantStatus: http.StatusOK,
		},
		{
			name:       "error: not found",
			listingID:  uuid.New().String(),
			wantStatus: http.StatusNotFound,
			wantCode:   "LISTING_NOT_FOUND",
		},
		{
			name:       "error: invalid id",
			listingID:  "not-a-uuid",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH(existingListing)
			r := buildListingRouter(ls)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/listings/"+tc.listingID, nil)
			req.Header.Set("X-User-Id", ownerID.String())
			req.Header.Set("X-Kyc-Tier", "1")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])
			}
		})
	}
}
