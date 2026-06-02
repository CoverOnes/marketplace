package domain

// ValidBidTransition returns true when a bid can transition from its current
// status to the requested target status. All terminal states (ACCEPTED, REJECTED,
// WITHDRAWN) are final — no further transitions are allowed.
func ValidBidTransition(from, to BidStatus) bool {
	if from == to {
		return false
	}

	if from == BidStatusPending {
		switch to {
		case BidStatusAccepted, BidStatusRejected, BidStatusWithdrawn:
			return true
		}
	}

	// All other transitions (from any terminal state, or unknown states) are invalid.
	return false
}

// IsTerminalBidStatus reports whether s is a terminal bid state.
func IsTerminalBidStatus(s BidStatus) bool {
	switch s {
	case BidStatusAccepted, BidStatusRejected, BidStatusWithdrawn:
		return true
	}

	return false
}
