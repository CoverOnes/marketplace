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
