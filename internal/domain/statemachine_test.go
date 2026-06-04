package domain_test

import (
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/stretchr/testify/assert"
)

func TestValidBidTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		from  domain.BidStatus
		to    domain.BidStatus
		valid bool
	}{
		// Legal transitions from PENDING
		{"pending -> accepted", domain.BidStatusPending, domain.BidStatusAccepted, true},
		{"pending -> rejected", domain.BidStatusPending, domain.BidStatusRejected, true},
		{"pending -> withdrawn", domain.BidStatusPending, domain.BidStatusWithdrawn, true},

		// Illegal: same state
		{"pending -> pending (no-op)", domain.BidStatusPending, domain.BidStatusPending, false},
		{"accepted -> accepted (no-op)", domain.BidStatusAccepted, domain.BidStatusAccepted, false},

		// Illegal: transitions from terminal states
		{"accepted -> rejected", domain.BidStatusAccepted, domain.BidStatusRejected, false},
		{"accepted -> withdrawn", domain.BidStatusAccepted, domain.BidStatusWithdrawn, false},
		{"accepted -> pending", domain.BidStatusAccepted, domain.BidStatusPending, false},
		{"rejected -> accepted", domain.BidStatusRejected, domain.BidStatusAccepted, false},
		{"rejected -> pending", domain.BidStatusRejected, domain.BidStatusPending, false},
		{"withdrawn -> pending", domain.BidStatusWithdrawn, domain.BidStatusPending, false},
		{"withdrawn -> accepted", domain.BidStatusWithdrawn, domain.BidStatusAccepted, false},

		// Illegal: unknown states
		{"unknown from", domain.BidStatus("UNKNOWN"), domain.BidStatusAccepted, false},
		{"unknown to", domain.BidStatusPending, domain.BidStatus("UNKNOWN"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := domain.ValidBidTransition(tc.from, tc.to)
			assert.Equal(t, tc.valid, got)
		})
	}
}

// TestValidTenderTransition exhaustively covers every allowed and every
// forbidden tender_status transition. The test name encodes the direction so
// a failure message immediately points to which edge is broken.
func TestValidTenderTransition(t *testing.T) {
	t.Parallel()

	// allStatuses is used to generate forbidden-transition matrix rows.
	allStatuses := []domain.TenderStatus{
		domain.TenderStatusOpen,
		domain.TenderStatusPartiallyStaffed,
		domain.TenderStatusExecuting,
		domain.TenderStatusSettling,
		domain.TenderStatusCompleted,
		domain.TenderStatusCancelled,
	}

	tests := []struct {
		name  string
		from  domain.TenderStatus
		to    domain.TenderStatus
		valid bool
	}{
		// --- Allowed transitions ---
		// OPEN → allowed destinations
		{"OPEN -> PARTIALLY_STAFFED", domain.TenderStatusOpen, domain.TenderStatusPartiallyStaffed, true},
		{"OPEN -> EXECUTING", domain.TenderStatusOpen, domain.TenderStatusExecuting, true},
		{"OPEN -> CANCELED", domain.TenderStatusOpen, domain.TenderStatusCancelled, true},
		// PARTIALLY_STAFFED → allowed destinations
		{"PARTIALLY_STAFFED -> OPEN", domain.TenderStatusPartiallyStaffed, domain.TenderStatusOpen, true},
		{"PARTIALLY_STAFFED -> EXECUTING", domain.TenderStatusPartiallyStaffed, domain.TenderStatusExecuting, true},
		{"PARTIALLY_STAFFED -> CANCELED", domain.TenderStatusPartiallyStaffed, domain.TenderStatusCancelled, true},
		// EXECUTING → allowed destinations
		{"EXECUTING -> SETTLING", domain.TenderStatusExecuting, domain.TenderStatusSettling, true},
		{"EXECUTING -> CANCELED", domain.TenderStatusExecuting, domain.TenderStatusCancelled, true},
		// SETTLING → allowed destinations
		{"SETTLING -> COMPLETED", domain.TenderStatusSettling, domain.TenderStatusCompleted, true},
		{"SETTLING -> CANCELED", domain.TenderStatusSettling, domain.TenderStatusCancelled, true},

		// --- Self-transitions always forbidden ---
		{"OPEN -> OPEN (self)", domain.TenderStatusOpen, domain.TenderStatusOpen, false},
		{"PARTIALLY_STAFFED -> PARTIALLY_STAFFED (self)", domain.TenderStatusPartiallyStaffed, domain.TenderStatusPartiallyStaffed, false},
		{"EXECUTING -> EXECUTING (self)", domain.TenderStatusExecuting, domain.TenderStatusExecuting, false},
		{"SETTLING -> SETTLING (self)", domain.TenderStatusSettling, domain.TenderStatusSettling, false},
		{"COMPLETED -> COMPLETED (self)", domain.TenderStatusCompleted, domain.TenderStatusCompleted, false},
		{"CANCELED -> CANCELED (self)", domain.TenderStatusCancelled, domain.TenderStatusCancelled, false},

		// --- Terminal states: no further transitions allowed ---
		{"COMPLETED -> OPEN", domain.TenderStatusCompleted, domain.TenderStatusOpen, false},
		{"COMPLETED -> PARTIALLY_STAFFED", domain.TenderStatusCompleted, domain.TenderStatusPartiallyStaffed, false},
		{"COMPLETED -> EXECUTING", domain.TenderStatusCompleted, domain.TenderStatusExecuting, false},
		{"COMPLETED -> SETTLING", domain.TenderStatusCompleted, domain.TenderStatusSettling, false},
		{"COMPLETED -> CANCELED", domain.TenderStatusCompleted, domain.TenderStatusCancelled, false},
		{"CANCELED -> OPEN", domain.TenderStatusCancelled, domain.TenderStatusOpen, false},
		{"CANCELED -> PARTIALLY_STAFFED", domain.TenderStatusCancelled, domain.TenderStatusPartiallyStaffed, false},
		{"CANCELED -> EXECUTING", domain.TenderStatusCancelled, domain.TenderStatusExecuting, false},
		{"CANCELED -> SETTLING", domain.TenderStatusCancelled, domain.TenderStatusSettling, false},
		{"CANCELED -> COMPLETED", domain.TenderStatusCancelled, domain.TenderStatusCompleted, false},

		// --- OPEN: explicitly forbidden skip/backwards ---
		{"OPEN -> SETTLING (skip)", domain.TenderStatusOpen, domain.TenderStatusSettling, false},
		{"OPEN -> COMPLETED (skip)", domain.TenderStatusOpen, domain.TenderStatusCompleted, false},

		// --- PARTIALLY_STAFFED: explicitly forbidden backwards/skip ---
		{"PARTIALLY_STAFFED -> SETTLING (skip)", domain.TenderStatusPartiallyStaffed, domain.TenderStatusSettling, false},
		{"PARTIALLY_STAFFED -> COMPLETED (skip)", domain.TenderStatusPartiallyStaffed, domain.TenderStatusCompleted, false},

		// --- EXECUTING: forbidden backwards/skip ---
		{"EXECUTING -> OPEN (backwards)", domain.TenderStatusExecuting, domain.TenderStatusOpen, false},
		{"EXECUTING -> PARTIALLY_STAFFED (backwards)", domain.TenderStatusExecuting, domain.TenderStatusPartiallyStaffed, false},
		{"EXECUTING -> COMPLETED (skip)", domain.TenderStatusExecuting, domain.TenderStatusCompleted, false},

		// --- SETTLING: forbidden backwards ---
		{"SETTLING -> OPEN (backwards)", domain.TenderStatusSettling, domain.TenderStatusOpen, false},
		{"SETTLING -> PARTIALLY_STAFFED (backwards)", domain.TenderStatusSettling, domain.TenderStatusPartiallyStaffed, false},
		{"SETTLING -> EXECUTING (backwards)", domain.TenderStatusSettling, domain.TenderStatusExecuting, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := domain.ValidTenderTransition(tc.from, tc.to)
			assert.Equal(t, tc.valid, got,
				"ValidTenderTransition(%q, %q) = %v, want %v",
				tc.from, tc.to, got, tc.valid)
		})
	}

	// Ensure we haven't missed any status as a "from" state in the matrix above.
	// This meta-test fails if a new status is added but not covered.
	t.Run("all statuses appear as from in at least one case", func(t *testing.T) {
		t.Parallel()

		seen := make(map[domain.TenderStatus]bool)
		for _, tc := range tests {
			seen[tc.from] = true
		}

		for _, s := range allStatuses {
			assert.True(t, seen[s], "status %q never appears as 'from' in transition tests", s)
		}
	})
}

func TestIsTerminalBidStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   domain.BidStatus
		terminal bool
	}{
		{"ACCEPTED is terminal", domain.BidStatusAccepted, true},
		{"REJECTED is terminal", domain.BidStatusRejected, true},
		{"WITHDRAWN is terminal", domain.BidStatusWithdrawn, true},
		{"PENDING is not terminal", domain.BidStatusPending, false},
		{"unknown is not terminal", domain.BidStatus("UNKNOWN"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := domain.IsTerminalBidStatus(tc.status)
			assert.Equal(t, tc.terminal, got)
		})
	}
}
