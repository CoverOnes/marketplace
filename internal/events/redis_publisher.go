package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/redis/go-redis/v9"
)

const channelBidAccepted = "marketplace.bid_accepted"

// RedisPublisher publishes events to Redis pub/sub channels.
type RedisPublisher struct {
	rdb *redis.Client
}

// NewRedisPublisher returns a RedisPublisher backed by the given Redis client.
func NewRedisPublisher(rdb *redis.Client) *RedisPublisher {
	return &RedisPublisher{rdb: rdb}
}

// PublishBidAccepted serializes the event and publishes it to Redis.
// Transport failures are returned to the caller (caller should log and continue —
// the awards row is the durable source of truth).
func (p *RedisPublisher) PublishBidAccepted(ctx context.Context, evt *domain.BidAcceptedEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal bid_accepted event: %w", err)
	}

	if err := p.rdb.Publish(ctx, channelBidAccepted, payload).Err(); err != nil {
		return fmt.Errorf("redis publish %s: %w", channelBidAccepted, err)
	}

	return nil
}
