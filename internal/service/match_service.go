package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
)

// Locked weights for the 5-dimension vendor match score (must sum to 1.0).
const (
	weightSkill  = 0.35
	weightFit    = 0.15
	weightRel    = 0.25
	weightCollab = 0.20
	weightComm   = 0.05
)

// matchMaxTopK is the absolute cap on NearestNeighborsWithDistance candidates.
const matchMaxTopK = 200

// ScoreBreakdown holds the per-dimension score for a single vendor.
// Each value is in [0.0, 1.0].
type ScoreBreakdown struct {
	Skill       float64
	Reliability float64
	Collab      float64
	Fit         float64
	Comm        float64
}

// VendorMatchResult is a single ranked vendor entry.
type VendorMatchResult struct {
	VendorUserID uuid.UUID
	OverallScore float64
	Breakdown    ScoreBreakdown
}

// MatchResult is the full response payload for GetMatches.
type MatchResult struct {
	TenderID uuid.UUID
	Partial  bool
	Results  []VendorMatchResult
}

// MatchService ranks vendors against a tender using cosine similarity on embeddings
// plus optional workspace statistics (reliability/collab/comm).
type MatchService struct {
	listings    store.ListingStore
	embeddings  store.EmbeddingStore
	statsClient client.WorkspaceStatsClient
}

// NewMatchService returns a MatchService.
// statsClient MUST be non-nil; inject &client.NoopWorkspaceStatsClient{} for the
// ship-phase partial mode.
func NewMatchService(
	listings store.ListingStore,
	embeddings store.EmbeddingStore,
	statsClient client.WorkspaceStatsClient,
) *MatchService {
	return &MatchService{
		listings:    listings,
		embeddings:  embeddings,
		statsClient: statsClient,
	}
}

// ComputeOverall combines a ScoreBreakdown into a single weighted score.
// Pure function — no side effects, safe to unit-test directly.
// Weights: skill .35 / fit .15 / reliability .25 / collab .20 / comm .05.
func ComputeOverall(b ScoreBreakdown) float64 {
	return b.Skill*weightSkill +
		b.Fit*weightFit +
		b.Reliability*weightRel +
		b.Collab*weightCollab +
		b.Comm*weightComm
}

// clampSimilarity converts a pgvector cosine distance d ∈ [0, 2] to a similarity
// score in [0, 1]. Cosine distance: 0 = identical, 2 = opposite.
// Formula: similarity = clamp(1 - d/2, 0, 1).
func clampSimilarity(cosineDistance float32) float64 {
	s := 1.0 - float64(cosineDistance)/2.0

	if s < 0 {
		return 0
	}

	if s > 1 {
		return 1
	}

	return s
}

// GetMatches returns a ranked vendor list for the given tender, applying
// owner-only access control, owner exclusion from results, and partial scoring
// when workspace stats are unavailable.
//
// Security invariants enforced here:
//   - Owner-only + tender guard: match results are private to the tender owner.
//     Classic listings (is_tender=false) and any caller who is not the owner
//     all receive ErrListingNotFound (404) — no resource enumeration.
//   - IDOR: excludes the tender owner from match results (owner cannot match
//     themselves as a vendor on their own tender).
//   - GET side-effect: no write — no ai_recommendation row is inserted.
func (s *MatchService) GetMatches(
	ctx context.Context,
	callerID uuid.UUID,
	tenderID uuid.UUID,
	limit int,
) (*MatchResult, error) {
	// Validate and clamp limit.
	if limit <= 0 {
		limit = 10
	}

	if limit > 50 {
		limit = 50
	}

	// Fetch listing; not-found errors propagate as-is (→ 404).
	listing, err := s.listings.GetByID(ctx, tenderID)
	if err != nil {
		return nil, err
	}

	// Match results are private to the tender owner; the endpoint serves tenders only.
	// Classic listings (IsTender=false) and non-owner callers → 404 (no enumeration).
	if !listing.IsTender || listing.OwnerUserID != callerID {
		return nil, domain.ErrListingNotFound
	}

	// Require a tender embedding; 422 if not yet indexed.
	tenderEmb, err := s.embeddings.GetByEntityID(ctx, domain.EmbeddingEntityTypeTender, tenderID)
	if err != nil {
		// ErrNotFound from GetByEntityID means no embedding row exists for this tender.
		// Map to ErrTenderNotIndexed so the handler can return 422.
		if errors.Is(err, domain.ErrNotFound) {
			return nil, domain.ErrTenderNotIndexed
		}

		return nil, fmt.Errorf("get tender embedding: %w", err)
	}

	// Over-fetch to absorb the owner-exclusion filter.
	topK := limit * 2
	if topK > matchMaxTopK {
		topK = matchMaxTopK
	}

	candidates, err := s.embeddings.NearestNeighborsWithDistance(
		ctx,
		tenderEmb.Vector,
		domain.EmbeddingEntityTypeVendor,
		topK,
	)
	if err != nil {
		return nil, fmt.Errorf("nearest neighbors with distance: %w", err)
	}

	partial := false
	results := make([]VendorMatchResult, 0, len(candidates))

	for _, c := range candidates {
		// Exclude the tender owner from match results (IDOR + business rule).
		if c.EntityID == listing.OwnerUserID {
			continue
		}

		skill := clampSimilarity(c.CosineDistance)
		fit := skill * skill // fit = skill² (v1 placeholder; documented in design)

		stats, statsErr := s.statsClient.GetVendorStats(ctx, c.EntityID.String())
		if statsErr != nil {
			// Treat stats error as unavailable; degrade gracefully.
			stats = client.VendorStats{Available: false}
		}

		var breakdown ScoreBreakdown

		breakdown.Skill = skill
		breakdown.Fit = fit

		if stats.Available {
			breakdown.Reliability = stats.Reliability
			breakdown.Collab = stats.Collaboration
			breakdown.Comm = stats.Communication
		} else {
			partial = true
		}

		results = append(results, VendorMatchResult{
			VendorUserID: c.EntityID,
			OverallScore: ComputeOverall(breakdown),
			Breakdown:    breakdown,
		})
	}

	// Sort descending by overall score.
	sort.Slice(results, func(i, j int) bool {
		return results[i].OverallScore > results[j].OverallScore
	})

	// Trim to requested limit.
	if len(results) > limit {
		results = results[:limit]
	}

	return &MatchResult{
		TenderID: tenderID,
		Partial:  partial,
		Results:  results,
	}, nil
}
