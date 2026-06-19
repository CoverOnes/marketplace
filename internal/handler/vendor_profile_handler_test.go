package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/handler"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub store for vendor profile handler tests ---

type stubVendorProfileStoreH struct {
	profiles  map[uuid.UUID]*domain.VendorProfile
	upsertErr error
	getErr    error
}

func newStubVendorProfileStoreH() *stubVendorProfileStoreH {
	return &stubVendorProfileStoreH{profiles: make(map[uuid.UUID]*domain.VendorProfile)}
}

func (s *stubVendorProfileStoreH) Upsert(_ context.Context, p *domain.VendorProfile) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}

	s.profiles[p.OwnerUserID] = p

	return nil
}

func (s *stubVendorProfileStoreH) GetByOwner(_ context.Context, ownerUserID uuid.UUID) (*domain.VendorProfile, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}

	p, ok := s.profiles[ownerUserID]
	if !ok {
		return nil, domain.ErrNotFound
	}

	return p, nil
}

// buildVendorProfileRouter returns a minimal Gin engine with the two Slice-1 endpoints.
func buildVendorProfileRouter(st *stubVendorProfileStoreH) *gin.Engine {
	gin.SetMode(gin.TestMode)

	svc := service.NewVendorProfileService(st, nil)
	h := handler.NewVendorProfileHandler(svc)

	r := gin.New()

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())
	api.PUT("/vendor-profile", middleware.RequireTier(2), h.Upsert)
	api.GET("/vendor-profile", middleware.RequireTier(2), h.GetOwn)

	return r
}

// doRequest executes a request against the router and returns the recorder.
func doVendorRequest(r *gin.Engine, method, path, userID, tier string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}

	req := httptest.NewRequestWithContext(context.Background(), method, path, &buf)
	req.Header.Set("Content-Type", "application/json")

	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	}

	if tier != "" {
		req.Header.Set("X-Kyc-Tier", tier)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

// TestVendorProfileHandler_Upsert tests PUT /v1/vendor-profile.
func TestVendorProfileHandler_Upsert(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name       string
		userID     string
		tier       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{
			name:   "happy path: create profile",
			userID: ownerID.String(),
			tier:   "2",
			body: map[string]any{
				"displayName": "Alice Vendor",
				"skills":      []string{"Go", "PostgreSQL"},
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "happy path: upsert with all optional fields",
			userID: ownerID.String(),
			tier:   "2",
			body: map[string]any{
				"displayName": "Alice Vendor",
				"headline":    "Go specialist",
				"bio":         "I write Go services.\nMultiline bio is allowed.",
				"skills":      []string{"Go"},
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "error: missing X-User-Id → 401",
			userID:     "",
			tier:       "2",
			body:       map[string]any{"displayName": "Alice"},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
		},
		{
			name:       "error: Tier1 → 403 KYC_TIER_REQUIRED",
			userID:     ownerID.String(),
			tier:       "1",
			body:       map[string]any{"displayName": "Alice"},
			wantStatus: http.StatusForbidden,
			wantCode:   "KYC_TIER_REQUIRED",
		},
		{
			name:       "error: empty display_name → 400 VALIDATION_ERROR",
			userID:     ownerID.String(),
			tier:       "2",
			body:       map[string]any{"displayName": ""},
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:   "error: display_name with control char → 400 VALIDATION_ERROR",
			userID: ownerID.String(),
			tier:   "2",
			body: map[string]any{
				"displayName": "bad\x01name",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:   "error: display_name with null byte → 400 VALIDATION_ERROR",
			userID: ownerID.String(),
			tier:   "2",
			body: map[string]any{
				"displayName": "bad\x00name",
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := newStubVendorProfileStoreH()
			r := buildVendorProfileRouter(st)

			w := doVendorRequest(r, http.MethodPut, "/v1/vendor-profile", tc.userID, tc.tier, tc.body)
			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errObj, ok := resp["error"].(map[string]any)
				require.True(t, ok, "response must have 'error' object")
				assert.Equal(t, tc.wantCode, errObj["code"])
			}
		})
	}
}

// TestVendorProfileHandler_Upsert_OwnerFromIdentityOnly verifies that a
// body-supplied ownerUserId is completely ignored — the profile is always stored
// under the identity's UserID.
func TestVendorProfileHandler_Upsert_OwnerFromIdentityOnly(t *testing.T) {
	t.Parallel()

	realOwnerID := uuid.New()
	fakeOwnerID := uuid.New()

	st := newStubVendorProfileStoreH()
	r := buildVendorProfileRouter(st)

	// The body includes a bogus ownerUserId; it must be silently discarded.
	body := map[string]any{
		"displayName":   "Alice Vendor",
		"ownerUserId":   fakeOwnerID.String(), // must be ignored
		"ownerUserID":   fakeOwnerID.String(), // must be ignored (camelCase variant)
		"owner_user_id": fakeOwnerID.String(), // must be ignored (snake_case variant)
	}

	w := doVendorRequest(r, http.MethodPut, "/v1/vendor-profile", realOwnerID.String(), "2", body)
	require.Equal(t, http.StatusOK, w.Code)

	// The stored profile must be keyed by realOwnerID.
	assert.NotNil(t, st.profiles[realOwnerID], "profile must be stored under the real owner")
	assert.Nil(t, st.profiles[fakeOwnerID], "profile must NOT be stored under the fake owner")
}

// TestVendorProfileHandler_GetOwn tests GET /v1/vendor-profile.
func TestVendorProfileHandler_GetOwn(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name       string
		userID     string
		tier       string
		setup      func(*stubVendorProfileStoreH)
		wantStatus int
		wantCode   string
	}{
		{
			name:   "happy path: profile exists",
			userID: ownerID.String(),
			tier:   "2",
			setup: func(st *stubVendorProfileStoreH) {
				st.profiles[ownerID] = &domain.VendorProfile{
					ID:          uuid.New(),
					OwnerUserID: ownerID,
					DisplayName: "Alice Vendor",
					Skills:      []string{"Go"},
				}
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "error: no profile → 404 VENDOR_PROFILE_NOT_FOUND",
			userID:     ownerID.String(),
			tier:       "2",
			setup:      nil,
			wantStatus: http.StatusNotFound,
			wantCode:   "VENDOR_PROFILE_NOT_FOUND",
		},
		{
			name:       "error: missing X-User-Id → 401",
			userID:     "",
			tier:       "2",
			setup:      nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHORIZED",
		},
		{
			name:       "error: Tier1 → 403",
			userID:     ownerID.String(),
			tier:       "1",
			setup:      nil,
			wantStatus: http.StatusForbidden,
			wantCode:   "KYC_TIER_REQUIRED",
		},
		{
			name:   "error: internal store error → 500",
			userID: ownerID.String(),
			tier:   "2",
			setup: func(st *stubVendorProfileStoreH) {
				st.getErr = errors.New("db unavailable")
			},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := newStubVendorProfileStoreH()
			if tc.setup != nil {
				tc.setup(st)
			}

			r := buildVendorProfileRouter(st)
			w := doVendorRequest(r, http.MethodGet, "/v1/vendor-profile", tc.userID, tc.tier, nil)
			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errObj, ok := resp["error"].(map[string]any)
				require.True(t, ok, "response must have 'error' object")
				assert.Equal(t, tc.wantCode, errObj["code"])
			}
		})
	}
}
