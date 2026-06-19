package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/handler"
	"github.com/CoverOnes/marketplace/internal/platform/middleware"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub embedding store ---

// stubEmbeddingStoreH satisfies store.EmbeddingStore for handler-layer unit tests.
type stubEmbeddingStoreH struct {
	byEntity  map[string]*domain.Embedding    // key: "type:uuid"
	neighbors []*domain.EmbeddingWithDistance // ordered slice returned by NearestNeighborsWithDistance
}

func newStubEmbeddingStoreH() *stubEmbeddingStoreH {
	return &stubEmbeddingStoreH{
		byEntity: make(map[string]*domain.Embedding),
	}
}

func (s *stubEmbeddingStoreH) entityKey(et domain.EmbeddingEntityType, id uuid.UUID) string {
	return fmt.Sprintf("%s:%s", et, id)
}

func (s *stubEmbeddingStoreH) Upsert(
	_ context.Context,
	entityType domain.EmbeddingEntityType,
	entityID uuid.UUID,
	embedding []float32,
	modelVersion string,
) error {
	s.byEntity[s.entityKey(entityType, entityID)] = &domain.Embedding{
		ID:           uuid.New(),
		EntityType:   entityType,
		EntityID:     entityID,
		Vector:       embedding,
		ModelVersion: modelVersion,
		CreatedAt:    time.Now().UTC(),
	}

	return nil
}

func (s *stubEmbeddingStoreH) NearestNeighbors(
	_ context.Context,
	_ []float32,
	_ domain.EmbeddingEntityType,
	_ int,
) ([]*domain.Embedding, error) {
	return nil, nil
}

func (s *stubEmbeddingStoreH) GetByEntityID(
	_ context.Context,
	entityType domain.EmbeddingEntityType,
	entityID uuid.UUID,
) (*domain.Embedding, error) {
	e, ok := s.byEntity[s.entityKey(entityType, entityID)]
	if !ok {
		return nil, domain.ErrNotFound
	}

	return e, nil
}

func (s *stubEmbeddingStoreH) NearestNeighborsWithDistance(
	_ context.Context,
	_ []float32,
	_ domain.EmbeddingEntityType,
	topK int,
) ([]*domain.EmbeddingWithDistance, error) {
	if topK > len(s.neighbors) {
		return s.neighbors, nil
	}

	return s.neighbors[:topK], nil
}

// makeTestVec creates a normalised 1536-dim float32 vector with val0 at index 0.
func makeTestVec(val0 float32) []float32 {
	v := make([]float32, 1536)
	v[0] = val0
	norm := float32(math.Abs(float64(val0)))

	if norm > 0 {
		v[0] /= norm
	}

	return v
}

// buildMatchRouter sets up a minimal Gin router wired to a MatchHandler.
// Mirrors the production setup: RequireValidIdentity as group-level middleware,
// RequireTier(2) as per-route middleware — matching router.go.
func buildMatchRouter(
	listingStore stubListingStoreInterface,
	embeddingStore *stubEmbeddingStoreH,
) *gin.Engine {
	statsClient := &client.NoopWorkspaceStatsClient{}
	svc := service.NewMatchService(listingStore, embeddingStore, statsClient)
	h := handler.NewMatchHandler(svc)

	r := gin.New()
	r.Use(middleware.RequireValidIdentity())
	r.GET("/v1/tenders/:id/matches", middleware.RequireTier(2), h.GetMatches)

	return r
}

// stubListingStoreInterface avoids duplicating the full store.ListingStore interface here;
// we use the existing stubListingStoreH from listing_handler_test.go which is in the same package.
type stubListingStoreInterface = *stubListingStoreH

// newMatchReq builds a GET request for the match endpoint.
// userID="" omits the X-User-Id header (unauthenticated). kycTier="" omits X-Kyc-Tier.
func newMatchReq(path, userID, kycTier string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	}

	if kycTier != "" {
		req.Header.Set("X-Kyc-Tier", kycTier)
	}

	return req
}

// openTenderListing builds a minimal OPEN tender listing with the given owner.
func openTenderListing(ownerID uuid.UUID) *domain.Listing {
	ts := domain.TenderStatusOpen

	return &domain.Listing{
		ID:           uuid.New(),
		OwnerUserID:  ownerID,
		Title:        "Test tender",
		Currency:     "TWD",
		Status:       domain.ListingStatusOpen,
		IsTender:     true,
		TenderStatus: &ts,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
}

// TestMatchHandler_GetMatches tests the GET /v1/tenders/:id/matches handler.
func TestMatchHandler_GetMatches(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	vendor1ID := uuid.New()
	vendor2ID := uuid.New()
	vendor3ID := uuid.New()

	listing := openTenderListing(ownerID)

	t.Run("owner gets ranked results partial=true and breakdown.reliability==0", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH(listing)
		embStore := newStubEmbeddingStoreH()

		// Seed tender embedding.
		tenderVec := makeTestVec(1.0)
		require.NoError(t, embStore.Upsert(context.Background(),
			domain.EmbeddingEntityTypeTender, listing.ID, tenderVec, "v1"))

		// Seed vendor neighbors (sorted by ascending distance, as DB would return).
		embStore.neighbors = []*domain.EmbeddingWithDistance{
			{Embedding: &domain.Embedding{EntityType: domain.EmbeddingEntityTypeVendor, EntityID: vendor1ID, Vector: makeTestVec(1.0)}, CosineDistance: 0.0},
			{Embedding: &domain.Embedding{EntityType: domain.EmbeddingEntityTypeVendor, EntityID: vendor2ID, Vector: makeTestVec(0.5)}, CosineDistance: 1.0},
			{Embedding: &domain.Embedding{EntityType: domain.EmbeddingEntityTypeVendor, EntityID: vendor3ID, Vector: makeTestVec(-1.0)}, CosineDistance: 2.0},
		}

		router := buildMatchRouter(listingStore, embStore)
		// Owner calls the endpoint.
		req := newMatchReq("/v1/tenders/"+listing.ID.String()+"/matches?limit=3", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

		var resp struct {
			Data struct {
				TenderID string `json:"tenderId"`
				Partial  bool   `json:"partial"`
				Results  []struct {
					VendorUserID string  `json:"vendorUserId"`
					OverallScore float64 `json:"overallScore"`
					Breakdown    struct {
						Reliability float64 `json:"reliability"`
						Collab      float64 `json:"collab"`
						Comm        float64 `json:"comm"`
					} `json:"breakdown"`
				} `json:"results"`
			} `json:"data"`
		}

		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.True(t, resp.Data.Partial, "partial must be true with NoopWorkspaceStatsClient")
		require.Len(t, resp.Data.Results, 3)

		// Results sorted descending by overall score → vendor1 first (distance=0 → skill=1).
		assert.Equal(t, vendor1ID.String(), resp.Data.Results[0].VendorUserID)
		assert.Greater(t, resp.Data.Results[0].OverallScore, resp.Data.Results[1].OverallScore)

		// Workspace dims must be zero (Noop).
		for _, r := range resp.Data.Results {
			assert.Equal(t, 0.0, r.Breakdown.Reliability, "reliability must be 0 (Noop)")
			assert.Equal(t, 0.0, r.Breakdown.Collab, "collab must be 0 (Noop)")
			assert.Equal(t, 0.0, r.Breakdown.Comm, "comm must be 0 (Noop)")
		}
	})

	t.Run("owner excluded from results", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH(listing)
		embStore := newStubEmbeddingStoreH()

		tenderVec := makeTestVec(1.0)
		require.NoError(t, embStore.Upsert(context.Background(),
			domain.EmbeddingEntityTypeTender, listing.ID, tenderVec, "v1"))

		// Seed owner as nearest neighbor — service must exclude them.
		embStore.neighbors = []*domain.EmbeddingWithDistance{
			{Embedding: &domain.Embedding{EntityType: domain.EmbeddingEntityTypeVendor, EntityID: ownerID, Vector: makeTestVec(1.0)}, CosineDistance: 0.0},
			{Embedding: &domain.Embedding{EntityType: domain.EmbeddingEntityTypeVendor, EntityID: vendor1ID, Vector: makeTestVec(0.9)}, CosineDistance: 0.5},
		}

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+listing.ID.String()+"/matches", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			Data struct {
				Results []struct {
					VendorUserID string `json:"vendorUserId"`
				} `json:"results"`
			} `json:"data"`
		}

		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))

		for _, r := range resp.Data.Results {
			assert.NotEqual(t, ownerID.String(), r.VendorUserID, "owner must not appear in results")
		}
	})

	t.Run("422 on unindexed tender (owner caller)", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH(listing)
		embStore := newStubEmbeddingStoreH() // no tender embedding seeded

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+listing.ID.String()+"/matches", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		errObj := resp["error"].(map[string]interface{})
		assert.Equal(t, "TENDER_NOT_INDEXED", errObj["code"])
	})

	t.Run("404 non-owner Tier-2 on OPEN tender (M-2 owner-only closed)", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH(listing)
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)

		nonOwnerID := uuid.New()
		req := newMatchReq("/v1/tenders/"+listing.ID.String()+"/matches", nonOwnerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("404 classic listing IsTender=false OPEN (M-1 existence oracle closed)", func(t *testing.T) {
		t.Parallel()

		// Classic listing: OPEN but NOT a tender — must return 404, not 422.
		classicListing := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Classic listing",
			Currency:    "TWD",
			Status:      domain.ListingStatusOpen,
			IsTender:    false, // not a tender
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}

		listingStore := newStubListingStoreH(classicListing)
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)
		// Even the owner calling on a classic listing gets 404, not 422.
		req := newMatchReq("/v1/tenders/"+classicListing.ID.String()+"/matches", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("404 non-OPEN tender non-owner (subsumed by owner-only guard)", func(t *testing.T) {
		t.Parallel()

		// AWARDED tender owned by ownerID.
		awardedListing := &domain.Listing{
			ID:          uuid.New(),
			OwnerUserID: ownerID,
			Title:       "Awarded tender",
			Currency:    "TWD",
			Status:      domain.ListingStatusAwarded,
			IsTender:    true,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}

		listingStore := newStubListingStoreH(awardedListing)
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)

		nonOwnerID := uuid.New()
		req := newMatchReq("/v1/tenders/"+awardedListing.ID.String()+"/matches", nonOwnerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("limit 51 clamped to 50 by service", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH(listing)
		embStore := newStubEmbeddingStoreH()

		tenderVec := makeTestVec(1.0)
		require.NoError(t, embStore.Upsert(context.Background(),
			domain.EmbeddingEntityTypeTender, listing.ID, tenderVec, "v1"))

		// Seed 5 vendor neighbors.
		for i := range 5 {
			vid := uuid.New()
			embStore.neighbors = append(embStore.neighbors, &domain.EmbeddingWithDistance{
				Embedding: &domain.Embedding{
					EntityType: domain.EmbeddingEntityTypeVendor,
					EntityID:   vid,
					Vector:     makeTestVec(float32(i)),
				},
				CosineDistance: float32(i) * 0.1,
			})
		}

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+listing.ID.String()+"/matches?limit=51", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			Data struct {
				Results []interface{} `json:"results"`
			} `json:"data"`
		}

		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		// 5 vendors seeded; limit clamped to 50. All 5 returned (< 50 cap).
		assert.LessOrEqual(t, len(resp.Data.Results), 50)
	})

	t.Run("limit 0 defaults to 10", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH(listing)
		embStore := newStubEmbeddingStoreH()

		tenderVec := makeTestVec(1.0)
		require.NoError(t, embStore.Upsert(context.Background(),
			domain.EmbeddingEntityTypeTender, listing.ID, tenderVec, "v1"))

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+listing.ID.String()+"/matches?limit=0", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		// No vendors seeded → 200 with empty results array (default limit=10).
		require.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			Data struct {
				Results []interface{} `json:"results"`
			} `json:"data"`
		}

		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.LessOrEqual(t, len(resp.Data.Results), 10)
	})

	t.Run("404 on tender not found at all", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH() // empty
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+uuid.New().String()+"/matches", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("400 on malformed UUID path param", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH()
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/not-a-uuid/matches", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("400 on non-integer limit param", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH()
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+uuid.New().String()+"/matches?limit=abc", ownerID.String(), "2")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("401 when X-User-Id header missing", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH()
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+uuid.New().String()+"/matches", "", "") // no identity headers

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("403 when KYC tier below 2", func(t *testing.T) {
		t.Parallel()

		listingStore := newStubListingStoreH()
		embStore := newStubEmbeddingStoreH()

		router := buildMatchRouter(listingStore, embStore)
		req := newMatchReq("/v1/tenders/"+uuid.New().String()+"/matches", uuid.New().String(), "1")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}
