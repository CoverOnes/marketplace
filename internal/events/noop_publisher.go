package events

import (
	"context"
	"log/slog"

	"github.com/CoverOnes/marketplace/internal/domain"
)

// NoopPublisher is a pass-through Publisher used when Redis is not configured.
// It logs a warning so operators know events are not being delivered.
type NoopPublisher struct{}

// NewNoopPublisher returns a NoopPublisher.
func NewNoopPublisher() *NoopPublisher {
	return &NoopPublisher{}
}

// PublishBidAccepted logs the event and returns nil (best-effort pass-through).
func (p *NoopPublisher) PublishBidAccepted(_ context.Context, evt *domain.BidAcceptedEvent) error {
	slog.Warn(
		"noop publisher: marketplace.bid_accepted not delivered (Redis not configured)",
		"event_id", evt.EventID,
		"award_id", evt.Data.AwardID,
		"listing_id", evt.Data.ListingID,
	)

	return nil
}
