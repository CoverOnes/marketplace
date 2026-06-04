package domain

// ValidTenderTransition returns true when a tender_status can transition from
// its current value to the requested target. This is a pure function — no side
// effects — tested exhaustively over ALL forbidden transitions as well as the
// allowed ones.
//
// Phase 1 reachable transitions:
//
//	OPEN → PARTIALLY_STAFFED (first collaborator approved)
//	OPEN → EXECUTING         (all roles filled / owner advances)
//	PARTIALLY_STAFFED → OPEN (last approved collaborator exits)
//	PARTIALLY_STAFFED → EXECUTING
//
// SETTLING, COMPLETED, CANCELED are defined but only driven by later phases.
// Note: the DB enum value is the British-spelling "CANCELED" (see migrations/000005).
// Self-transitions (from == to) are always invalid.
func ValidTenderTransition(from, to TenderStatus) bool {
	if from == to {
		return false
	}

	switch from {
	case TenderStatusOpen:
		switch to {
		case TenderStatusPartiallyStaffed, TenderStatusExecuting, TenderStatusCancelled:
			return true
		}

	case TenderStatusPartiallyStaffed:
		switch to {
		case TenderStatusOpen, TenderStatusExecuting, TenderStatusCancelled:
			return true
		}

	case TenderStatusExecuting:
		switch to {
		case TenderStatusSettling, TenderStatusCancelled:
			return true
		}

	case TenderStatusSettling:
		switch to {
		case TenderStatusCompleted, TenderStatusCancelled:
			return true
		}

	case TenderStatusCompleted, TenderStatusCancelled:
		// Terminal states — no further transitions.
		return false
	}

	return false
}

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
