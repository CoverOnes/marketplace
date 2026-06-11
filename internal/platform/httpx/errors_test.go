package httpx_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/platform/httpx"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// callErr invokes httpx.Err with the given error via a minimal gin test context
// and returns the HTTP status code plus the decoded error response.
func callErr(t *testing.T, domainErr error) (int, httpx.ErrorBody) {
	t.Helper()

	w := httptest.NewRecorder()
	_, engine := gin.CreateTestContext(w)

	engine.GET("/", func(ctx *gin.Context) {
		httpx.Err(ctx, domainErr)
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	engine.ServeHTTP(w, req)

	var resp httpx.ErrorResponse

	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))

	return w.Code, resp.Error
}

// TestTranslate_ErrUpstreamWorkspace verifies that ErrUpstreamWorkspace maps to 502.
func TestTranslate_ErrUpstreamWorkspace(t *testing.T) {
	t.Parallel()

	status, body := callErr(t, domain.ErrUpstreamWorkspace)

	assert.Equal(t, http.StatusBadGateway, status)
	assert.Equal(t, "UPSTREAM_WORKSPACE_ERROR", body.Code)
}

// TestTranslate_ErrInvalidTenderTransition verifies that ErrInvalidTenderTransition maps to 409.
func TestTranslate_ErrInvalidTenderTransition(t *testing.T) {
	t.Parallel()

	status, body := callErr(t, domain.ErrInvalidTenderTransition)

	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "INVALID_TENDER_TRANSITION", body.Code)
}

// TestTranslate_AllTenderErrors verifies the complete tender error → HTTP mapping table.
// This is a regression guard: adding a new domain error without updating httpx will cause
// it to fall through to 500, which is caught here.
func TestTranslate_AllTenderErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "ErrTenderBidNotAllowed → 409",
			err:        domain.ErrTenderBidNotAllowed,
			wantStatus: http.StatusConflict,
			wantCode:   "TENDER_BID_NOT_ALLOWED",
		},
		{
			name:       "ErrTenderRoleNotFound → 404",
			err:        domain.ErrTenderRoleNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "TENDER_ROLE_NOT_FOUND",
		},
		{
			name:       "ErrTenderCollaboratorNotFound → 404",
			err:        domain.ErrTenderCollaboratorNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "TENDER_COLLABORATOR_NOT_FOUND",
		},
		{
			name:       "ErrTenderRoleFilled → 409",
			err:        domain.ErrTenderRoleFilled,
			wantStatus: http.StatusConflict,
			wantCode:   "TENDER_ROLE_FILLED",
		},
		{
			name:       "ErrInvalidTenderTransition → 409",
			err:        domain.ErrInvalidTenderTransition,
			wantStatus: http.StatusConflict,
			wantCode:   "INVALID_TENDER_TRANSITION",
		},
		{
			name:       "ErrUpstreamWorkspace → 502",
			err:        domain.ErrUpstreamWorkspace,
			wantStatus: http.StatusBadGateway,
			wantCode:   "UPSTREAM_WORKSPACE_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, body := callErr(t, tc.err)
			assert.Equal(t, tc.wantStatus, status, "HTTP status for %s", tc.err)
			assert.Equal(t, tc.wantCode, body.Code, "code for %s", tc.err)
		})
	}
}
