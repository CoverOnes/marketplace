package service

import (
	"sort"

	"github.com/google/uuid"
)

// rrfK is the Reciprocal Rank Fusion constant. k=60 is the standard value
// from the original Cormack et al. 2009 paper; higher k narrows rank differences.
const rrfK = 60

// RRFCombine fuses lexical and semantic result lists using Reciprocal Rank Fusion.
//
// Both lists must be already ordered from best to worst; index 0 is rank 1.
// Duplicate IDs across lists accumulate scores; IDs present in only one list
// still get a score from that list alone (no penalty for absence).
//
// The returned slice is ordered by descending RRF score (highest first).
// Ties are broken by UUID string value for determinism.
//
// RRF score: score(d) = Σ_lists 1 / (rrfK + rank_i(d))
func RRFCombine(lexical, semantic []uuid.UUID) []uuid.UUID {
	scores := make(map[uuid.UUID]float64, len(lexical)+len(semantic))

	for i, id := range lexical {
		scores[id] += 1.0 / float64(rrfK+i+1) // rank is 1-indexed, i is 0-indexed
	}

	for i, id := range semantic {
		scores[id] += 1.0 / float64(rrfK+i+1)
	}

	// Collect unique IDs.
	ids := make([]uuid.UUID, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}

	// Sort by descending score; ties broken by UUID string for determinism.
	sort.Slice(ids, func(i, j int) bool {
		si, sj := scores[ids[i]], scores[ids[j]]
		if si != sj {
			return si > sj
		}

		return ids[i].String() < ids[j].String()
	})

	return ids
}
