// Package events provides event publishing for the marketplace service.
package events

import (
	"context"

	"github.com/CoverOnes/marketplace/internal/domain"
)

// Publisher publishes domain events to a transport (Redis pub/sub).
// Implementations must be safe for concurrent use.
type Publisher interface {
	// PublishBidAccepted sends the marketplace.bid_accepted event.
	// Best-effort: callers MUST NOT treat a publish failure as a reason to
	// roll back the accept transaction. The awards row is the authoritative record.
	PublishBidAccepted(ctx context.Context, evt *domain.BidAcceptedEvent) error
}
