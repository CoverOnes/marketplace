package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestRequireValidIdentity(t *testing.T) {
	t.Parallel()

	validID := uuid.New().String()

	tests := []struct {
		name          string
		userIDHeader  string
		kycTierHeader string
		wantStatus    int
		wantBodyCode  string
		wantIDInCtx   bool
	}{
		{
			name:         "happy path: valid uuid header",
			userIDHeader: validID,
			wantStatus:   http.StatusOK,
			wantIDInCtx:  true,
		},
		{
			name:          "happy path: valid uuid with tier",
			userIDHeader:  validID,
			kycTierHeader: "2",
			wantStatus:    http.StatusOK,
			wantIDInCtx:   true,
		},
		{
			name:         "error: missing X-User-Id",
			userIDHeader: "",
			wantStatus:   http.StatusUnauthorized,
			wantBodyCode: "UNAUTHORIZED",
		},
		{
			name:         "error: X-User-Id not a UUID",
			userIDHeader: "not-a-uuid",
			wantStatus:   http.StatusUnauthorized,
			wantBodyCode: "UNAUTHORIZED",
		},
		{
			name:         "error: X-User-Id is just whitespace",
			userIDHeader: "   ",
			wantStatus:   http.StatusUnauthorized,
			wantBodyCode: "UNAUTHORIZED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var capturedID uuid.UUID

			r := gin.New()
			r.Use(middleware.RequireValidIdentity())
			r.GET("/test", func(c *gin.Context) {
				id, ok := middleware.IdentityFromCtx(c)
				require.True(t, ok)
				capturedID = id.UserID
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)

			if tc.userIDHeader != "" {
				req.Header.Set("X-User-Id", tc.userIDHeader)
			}

			if tc.kycTierHeader != "" {
				req.Header.Set("X-Kyc-Tier", tc.kycTierHeader)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)

			if tc.wantIDInCtx {
				parsedID, err := uuid.Parse(validID)
				require.NoError(t, err)
				assert.Equal(t, parsedID, capturedID)
			}
		})
	}
}

func TestRequireTier(t *testing.T) {
	t.Parallel()

	validID := uuid.New().String()

	tests := []struct {
		name          string
		kycTierHeader string
		requiredTier  int
		wantStatus    int
		wantBodyCode  string
	}{
		{
			name:          "happy path: tier meets requirement",
			kycTierHeader: "2",
			requiredTier:  2,
			wantStatus:    http.StatusOK,
		},
		{
			name:          "happy path: tier exceeds requirement",
			kycTierHeader: "3",
			requiredTier:  2,
			wantStatus:    http.StatusOK,
		},
		{
			name:          "error: tier below requirement",
			kycTierHeader: "1",
			requiredTier:  2,
			wantStatus:    http.StatusForbidden,
			wantBodyCode:  "KYC_TIER_REQUIRED",
		},
		{
			name:          "error: tier 0, requires 1",
			kycTierHeader: "0",
			requiredTier:  1,
			wantStatus:    http.StatusForbidden,
			wantBodyCode:  "KYC_TIER_REQUIRED",
		},
		{
			name:          "error: missing tier defaults to 0",
			kycTierHeader: "",
			requiredTier:  2,
			wantStatus:    http.StatusForbidden,
			wantBodyCode:  "KYC_TIER_REQUIRED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := gin.New()
			r.Use(middleware.RequireValidIdentity())
			r.Use(middleware.RequireTier(tc.requiredTier))
			r.GET("/test", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
			req.Header.Set("X-User-Id", validID)

			if tc.kycTierHeader != "" {
				req.Header.Set("X-Kyc-Tier", tc.kycTierHeader)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)
		})
	}
}
