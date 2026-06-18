package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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

// List mirrors the SQL store: applies the visibility rule (OPEN is public,
// non-OPEN only to its owner via VisibleToUserID), plus optional status / owner
// filters. This lets the handler test exercise the P0 IDOR fix end-to-end.
func (s *stubListingStoreH) List(_ context.Context, filter store.ListingFilter) ([]*domain.Listing, error) {
	var result []*domain.Listing

	for _, l := range s.listings {
		// Visibility (mirrors SQL "(status = 'OPEN' OR owner_user_id = $n)").
		if l.Status != domain.ListingStatusOpen && l.OwnerUserID != filter.VisibleToUserID {
			continue
		}

		if filter.Status != nil && l.Status != *filter.Status {
			continue
		}

		if filter.OwnerUserID != nil && l.OwnerUserID != *filter.OwnerUserID {
			continue
		}

		result = append(result, l)
	}

	return result, nil
}

// Search implements a minimal in-memory analog of the SQL search: case-insensitive
// substring match on title+description, optional status + budget filters, keyset
// pagination on (created_at, id) ordered newest-first. Good enough to exercise the
// handler/service wiring without a database.
func (s *stubListingStoreH) Search(_ context.Context, filter store.SearchFilter) ([]*domain.Listing, error) {
	matched := make([]*domain.Listing, 0, len(s.listings))

	for _, l := range s.listings {
		if !stubSearchMatch(l, filter) {
			continue
		}

		matched = append(matched, l)
	}

	// Newest-first, id as tiebreaker (descending) — mirrors the SQL ORDER BY.
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}

		return matched[i].ID.String() > matched[j].ID.String()
	})

	// Apply keyset cursor: keep rows strictly older than the cursor position.
	if filter.After != nil {
		var page []*domain.Listing

		for _, l := range matched {
			if l.CreatedAt.Before(filter.After.CreatedAt) ||
				(l.CreatedAt.Equal(filter.After.CreatedAt) && l.ID.String() < filter.After.ID.String()) {
				page = append(page, l)
			}
		}

		matched = page
	}

	if filter.Limit > 0 && len(matched) > filter.Limit {
		matched = matched[:filter.Limit]
	}

	return matched, nil
}

func stubSearchMatch(l *domain.Listing, filter store.SearchFilter) bool {
	// Visibility (mirrors SQL): OPEN is public; non-OPEN only to its owner.
	if l.Status != domain.ListingStatusOpen && l.OwnerUserID != filter.VisibleToUserID {
		return false
	}

	if filter.Query != "" {
		// Approximate plainto_tsquery('simple', ...) AND-of-lexemes semantics:
		// the 'simple' config does NOT stem, so each query token must appear as a
		// whole word (lexeme) in title+description — "go" matches "go" but not
		// "golang". We tokenise on non-alphanumeric boundaries to mirror that.
		hayTokens := tokenize(l.Title + " " + l.Description)
		for tok := range tokenize(filter.Query) {
			if !hayTokens[tok] {
				return false
			}
		}
	}

	if filter.Status != nil && l.Status != *filter.Status {
		return false
	}

	if filter.BudgetMin != nil && l.BudgetMax != nil && l.BudgetMax.LessThan(*filter.BudgetMin) {
		return false
	}

	if filter.BudgetMax != nil && l.BudgetMin != nil && l.BudgetMin.GreaterThan(*filter.BudgetMax) {
		return false
	}

	return true
}

// tokenize lowercases s and splits it into the set of alphanumeric word tokens,
// approximating the 'simple' text-search configuration's lexeme extraction.
func tokenize(s string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		out[f] = true
	}

	return out
}

// GetByIDs returns the listings for the given IDs in order, visibility-filtered.
func (s *stubListingStoreH) GetByIDs(_ context.Context, ids []uuid.UUID, viewerID uuid.UUID) ([]*domain.Listing, error) {
	out := make([]*domain.Listing, 0, len(ids))

	for _, id := range ids {
		l, ok := s.listings[id]
		if !ok {
			continue
		}

		// Visibility: OPEN is public; non-OPEN only to owner.
		if l.Status != domain.ListingStatusOpen && l.OwnerUserID != viewerID {
			continue
		}

		out = append(out, l)
	}

	return out, nil
}

func (s *stubListingStoreH) Update(_ context.Context, l *domain.Listing) error {
	s.listings[l.ID] = l
	return nil
}

// buildListingRouter builds a router with ListingHandler wired.
func buildListingRouter(ls *stubListingStoreH) *gin.Engine {
	gin.SetMode(gin.TestMode)

	svc := service.NewListingService(ls, nil, nil, nil) // nil outbox/emb: no embedding in handler tests
	h := handler.NewListingHandler(svc)

	r := gin.New()

	api := r.Group("/v1")
	api.Use(middleware.RequireValidIdentity())
	api.POST("/listings", middleware.RequireTier(2), h.Create)
	api.GET("/listings", middleware.RequireTier(1), h.List)
	api.GET("/listings/search", middleware.RequireTier(1), h.Search)
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
	strangerID := uuid.New()

	openID := uuid.New()
	awardedID := uuid.New()

	openListing := &domain.Listing{
		ID:          openID,
		OwnerUserID: ownerID,
		Title:       "Existing OPEN",
		Currency:    "TWD",
		Status:      domain.ListingStatusOpen,
		BudgetMin:   func() *decimal.Decimal { d := decimal.NewFromInt(100); return &d }(),
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	// A non-OPEN listing owned by ownerID — must be invisible to a stranger.
	awardedListing := &domain.Listing{
		ID:          awardedID,
		OwnerUserID: ownerID,
		Title:       "Awarded private",
		Currency:    "TWD",
		Status:      domain.ListingStatusAwarded,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	tests := []struct {
		name       string
		listingID  string
		callerID   uuid.UUID
		wantStatus int
		wantCode   string
	}{
		{
			name:       "happy path: OPEN listing visible to owner",
			listingID:  openID.String(),
			callerID:   ownerID,
			wantStatus: http.StatusOK,
		},
		{
			name:       "happy path: OPEN listing visible to stranger",
			listingID:  openID.String(),
			callerID:   strangerID,
			wantStatus: http.StatusOK,
		},
		{
			name:       "happy path: AWARDED listing visible to owner",
			listingID:  awardedID.String(),
			callerID:   ownerID,
			wantStatus: http.StatusOK,
		},
		{
			name:       "IDOR: non-owner gets 404 on another's AWARDED listing",
			listingID:  awardedID.String(),
			callerID:   strangerID,
			wantStatus: http.StatusNotFound,
			wantCode:   "LISTING_NOT_FOUND",
		},
		{
			name:       "error: not found",
			listingID:  uuid.New().String(),
			callerID:   ownerID,
			wantStatus: http.StatusNotFound,
			wantCode:   "LISTING_NOT_FOUND",
		},
		{
			name:       "error: invalid id",
			listingID:  "not-a-uuid",
			callerID:   ownerID,
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH(openListing, awardedListing)
			r := buildListingRouter(ls)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/listings/"+tc.listingID, nil)
			req.Header.Set("X-User-Id", tc.callerID.String())
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

// TestListingHandler_List_StatusVisibility verifies the P0 IDOR fix for the list
// endpoint: ?status=AWARDED|CLOSED must only return the caller's own non-OPEN
// listings, never other users'.
func TestListingHandler_List_StatusVisibility(t *testing.T) {
	t.Parallel()

	callerID := uuid.New()
	strangerID := uuid.New()
	base := time.Now().UTC()

	myAwarded := mkListing(callerID, "My awarded", "", "AWARDED", base.Add(-1*time.Minute))
	strangerAwarded := mkListing(strangerID, "Stranger awarded", "", "AWARDED", base.Add(-2*time.Minute))
	strangerClosed := mkListing(strangerID, "Stranger closed", "", "CLOSED", base.Add(-3*time.Minute))
	myOpen := mkListing(callerID, "My open", "", "OPEN", base.Add(-4*time.Minute))
	strangerOpen := mkListing(strangerID, "Stranger open", "", "OPEN", base.Add(-5*time.Minute))

	titlesOf := func(body []byte) []string {
		var resp struct {
			Data []struct {
				Title string `json:"title"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(body, &resp))

		out := make([]string, 0, len(resp.Data))
		for _, l := range resp.Data {
			out = append(out, l.Title)
		}

		sort.Strings(out)

		return out
	}

	tests := []struct {
		name       string
		query      string
		wantTitles []string
	}{
		{
			name:       "status=AWARDED returns only caller's own awarded listing",
			query:      "?status=AWARDED",
			wantTitles: []string{"My awarded"},
		},
		{
			name:       "status=CLOSED hides stranger's closed listing",
			query:      "?status=CLOSED",
			wantTitles: []string{},
		},
		{
			name:       "default (no status) returns all OPEN listings from everyone",
			query:      "",
			wantTitles: []string{"My open", "Stranger open"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH(myAwarded, strangerAwarded, strangerClosed, myOpen, strangerOpen)
			r := buildListingRouter(ls)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/listings"+tc.query, nil)
			req.Header.Set("X-User-Id", callerID.String())
			req.Header.Set("X-Kyc-Tier", "1")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, tc.wantTitles, titlesOf(w.Body.Bytes()))
		})
	}
}

// searchDataResp models the GET /v1/listings/search success envelope.
type searchDataResp struct {
	Data struct {
		Listings []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"listings"`
		NextCursor string `json:"nextCursor"`
	} `json:"data"`
}

func mkListing(owner uuid.UUID, title, desc, status string, createdAt time.Time) *domain.Listing {
	return &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: owner,
		Title:       title,
		Description: desc,
		Currency:    "TWD",
		Status:      domain.ListingStatus(status),
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
}

func TestListingHandler_Search(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	base := time.Now().UTC().Truncate(time.Millisecond)

	// Three OPEN "go developer" listings + one unrelated + one CLOSED owned by a stranger.
	go1 := mkListing(ownerID, "Senior Go developer wanted", "build a payments API", "OPEN", base.Add(-1*time.Minute))
	go2 := mkListing(ownerID, "Golang backend engineer", "go microservices", "OPEN", base.Add(-2*time.Minute))
	go3 := mkListing(ownerID, "Need a Go developer", "concurrency expert", "OPEN", base.Add(-3*time.Minute))
	unrelated := mkListing(ownerID, "React frontend designer", "tailwind ui", "OPEN", base.Add(-4*time.Minute))
	closedStranger := mkListing(uuid.New(), "Go developer (closed)", "already filled", "CLOSED", base.Add(-30*time.Second))

	tests := []struct {
		name        string
		query       string
		wantStatus  int
		wantCode    string
		wantTitles  []string // expected titles in order (when 200)
		wantHasNext bool
	}{
		{
			name:       "happy path: keyword matches OPEN go listings, excludes unrelated + stranger CLOSED",
			query:      "q=go+developer&limit=10",
			wantStatus: http.StatusOK,
			// 'simple' config does not stem: "go developer" needs both lexemes.
			// go2 ("Golang ... go microservices") lacks "developer"; unrelated lacks
			// "go developer"; closedStranger is CLOSED+stranger (hidden by visibility).
			wantTitles: []string{go1.Title, go3.Title},
		},
		{
			name:        "pagination: limit 2 returns first page + nextCursor",
			query:       "q=go&limit=2",
			wantStatus:  http.StatusOK,
			wantTitles:  []string{go1.Title, go2.Title},
			wantHasNext: true,
		},
		{
			name:       "status filter: OPEN only",
			query:      "q=developer&status=OPEN&limit=10",
			wantStatus: http.StatusOK,
			wantTitles: []string{go1.Title, go3.Title},
		},
		{
			name:       "error: invalid status filter -> 400",
			query:      "status=BOGUS",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "error: minBudget not a decimal -> 400",
			query:      "minBudget=abc",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "error: negative limit -> 400",
			query:      "limit=-5",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name: "error: malformed cursor -> 400",
			// '.' is not in the base64url alphabet, so RawURLEncoding rejects it.
			query:      "cursor=invalid.cursor.token",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ls := newStubListingStoreH(go1, go2, go3, unrelated, closedStranger)
			r := buildListingRouter(ls)

			req := httptest.NewRequestWithContext(
				context.Background(), http.MethodGet, "/v1/listings/search?"+tc.query, nil,
			)
			req.Header.Set("X-User-Id", ownerID.String())
			req.Header.Set("X-Kyc-Tier", "1")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, tc.wantStatus, w.Code, "body: %s", w.Body.String())

			if tc.wantCode != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				errBody, ok := resp["error"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.wantCode, errBody["code"])

				return
			}

			var resp searchDataResp
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

			gotTitles := make([]string, 0, len(resp.Data.Listings))
			for _, l := range resp.Data.Listings {
				gotTitles = append(gotTitles, l.Title)
			}

			assert.Equal(t, tc.wantTitles, gotTitles)

			if tc.wantHasNext {
				assert.NotEmpty(t, resp.Data.NextCursor, "expected a nextCursor")
			} else {
				assert.Empty(t, resp.Data.NextCursor, "expected no nextCursor")
			}
		})
	}
}

// TestListingHandler_Search_CursorRoundTrip walks the full result set using the
// returned nextCursor and asserts the second page continues without overlap.
func TestListingHandler_Search_CursorRoundTrip(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	base := time.Now().UTC().Truncate(time.Millisecond)

	go1 := mkListing(ownerID, "Go developer one", "", "OPEN", base.Add(-1*time.Minute))
	go2 := mkListing(ownerID, "Go developer two", "", "OPEN", base.Add(-2*time.Minute))
	go3 := mkListing(ownerID, "Go developer three", "", "OPEN", base.Add(-3*time.Minute))

	ls := newStubListingStoreH(go1, go2, go3)
	r := buildListingRouter(ls)

	doPage := func(cursor string) searchDataResp {
		q := "q=go&limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}

		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/listings/search?"+q, nil)
		req.Header.Set("X-User-Id", ownerID.String())
		req.Header.Set("X-Kyc-Tier", "1")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

		var resp searchDataResp
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

		return resp
	}

	page1 := doPage("")
	require.Len(t, page1.Data.Listings, 2)
	require.NotEmpty(t, page1.Data.NextCursor)
	assert.Equal(t, go1.Title, page1.Data.Listings[0].Title)
	assert.Equal(t, go2.Title, page1.Data.Listings[1].Title)

	page2 := doPage(page1.Data.NextCursor)
	require.Len(t, page2.Data.Listings, 1)
	assert.Empty(t, page2.Data.NextCursor)
	assert.Equal(t, go3.Title, page2.Data.Listings[0].Title)
}
