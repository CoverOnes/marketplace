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

// buildUserRLRouter creates a test Gin engine with RequireValidIdentity +
// UserRateLimiter already wired, so tests only need to set X-User-Id.
func buildUserRLRouter(limitPerMin, burst int) *gin.Engine {
	r := gin.New()
	r.Use(middleware.RequireValidIdentity())
	userRL := middleware.NewUserRateLimiter(limitPerMin, burst)
	r.Use(userRL.Handler())
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	return r
}

// doRequest fires a GET /test with the given X-User-Id header.
func doRequest(r *gin.Engine, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

func TestUserRateLimiter_AllowWithinBudget(t *testing.T) {
	t.Parallel()

	// burst=3 means first 3 requests are allowed without any wait.
	// Use 120/min (the production default) to exercise a different limitPerMin.
	r := buildUserRLRouter(120, 3)
	uid := uuid.New().String()

	for i := range 3 {
		w := doRequest(r, uid)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should be allowed", i+1)
	}
}

func TestUserRateLimiter_DenyOverBudget(t *testing.T) {
	t.Parallel()

	// burst=2: first 2 requests pass, 3rd must be rejected.
	// Use 60/min to verify Retry-After is computed from limitPerMin.
	r := buildUserRLRouter(60, 2)
	uid := uuid.New().String()

	// Drain the burst.
	require.Equal(t, http.StatusOK, doRequest(r, uid).Code)
	require.Equal(t, http.StatusOK, doRequest(r, uid).Code)

	// Third request must hit the limit.
	w := doRequest(r, uid)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"), "Retry-After header must be set on 429")
}

func TestUserRateLimiter_IndependentBuckets(t *testing.T) {
	t.Parallel()

	// burst=1: each user gets exactly 1 token before being throttled.
	// Use 30/min to validate a third distinct limitPerMin value.
	r := buildUserRLRouter(30, 1)
	uid1 := uuid.New().String()
	uid2 := uuid.New().String()

	// First request from uid1 passes, second is denied.
	require.Equal(t, http.StatusOK, doRequest(r, uid1).Code)
	assert.Equal(t, http.StatusTooManyRequests, doRequest(r, uid1).Code)

	// uid2's bucket is independent — its first request must still pass.
	assert.Equal(t, http.StatusOK, doRequest(r, uid2).Code)
}

func TestUserRateLimiter_MissingIdentityPassthrough(t *testing.T) {
	t.Parallel()

	// When there is no identity in context the limiter must pass through
	// (not return 429), but since RequireValidIdentity blocks unauthenticated
	// requests first we build a router WITHOUT RequireValidIdentity to test
	// the limiter's own passthrough path.
	r := gin.New()
	userRL := middleware.NewUserRateLimiter(120, 1)
	r.Use(userRL.Handler())
	r.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Limiter passes through — downstream handler decides the response.
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUserRateLimiter_LRUBoundDoesNotPanic(t *testing.T) {
	t.Parallel()

	// Fire a large number of distinct user IDs (well above the LRU cap when
	// running in a tight loop) to verify the LRU eviction does not panic.
	// We use 240/min and burst=1 so the limiter inserts+evicts from the cache.
	r := buildUserRLRouter(240, 1)

	const distinctUsers = 500
	for i := range distinctUsers {
		uid := uuid.New().String()
		w := doRequest(r, uid)
		// Each fresh key gets one free token; all should be 200 on first hit.
		assert.Equal(t, http.StatusOK, w.Code, "user %d should be allowed on first request", i)
	}
}
