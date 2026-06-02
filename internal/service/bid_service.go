package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// BidService handles bid business logic including the accept transaction.
type BidService struct {
	bids      store.BidStore
	listings  store.ListingStore
	awards    store.AwardStore
	txManager store.TxManager
	publisher events.Publisher
}

// NewBidService returns a BidService.
func NewBidService(
	bids store.BidStore,
	listings store.ListingStore,
	awards store.AwardStore,
	txManager store.TxManager,
	publisher events.Publisher,
) *BidService {
	return &BidService{
		bids:      bids,
		listings:  listings,
		awards:    awards,
		txManager: txManager,
		publisher: publisher,
	}
}

// CreateBidInput carries validated input for creating a bid.
type CreateBidInput struct {
	ListingID    uuid.UUID
	BidderUserID uuid.UUID // set from X-User-Id header only
	Amount       decimal.Decimal
	Currency     string
	Message      string
}

// CreateBid places a bid on a listing.
// Guards: listing must exist and be OPEN; bidder must not be the listing owner;
// no existing PENDING bid for this (listing, bidder) pair.
func (s *BidService) CreateBid(ctx context.Context, in *CreateBidInput) (*domain.Bid, error) {
	// Validate amount > 0.
	if in.Amount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("%w: amount must be greater than 0", domain.ErrValidation)
	}

	// Validate amount does not exceed numeric(14,2) column max (MK-M2).
	if in.Amount.GreaterThan(maxNumeric14_2) {
		return nil, fmt.Errorf("%w: amount exceeds maximum allowed value", domain.ErrValidation)
	}

	if len(in.Currency) != 3 {
		return nil, fmt.Errorf("%w: currency must be a 3-letter code", domain.ErrValidation)
	}

	if err := sanitizeMessage(in.Message); err != nil {
		return nil, fmt.Errorf("%w: message: %s", domain.ErrValidation, err)
	}

	// Load listing to validate it exists and is OPEN.
	listing, err := s.listings.GetByID(ctx, in.ListingID)
	if err != nil {
		return nil, err
	}

	if listing.Status != domain.ListingStatusOpen {
		return nil, domain.ErrListingNotOpen
	}

	// Reject bidding on own listing.
	if listing.OwnerUserID == in.BidderUserID {
		return nil, domain.ErrBidOnOwnListing
	}

	now := time.Now().UTC()
	b := &domain.Bid{
		ID:           uuid.New(),
		ListingID:    in.ListingID,
		BidderUserID: in.BidderUserID,
		Amount:       in.Amount,
		Currency:     in.Currency,
		Message:      in.Message,
		Status:       domain.BidStatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.bids.Create(ctx, b); err != nil {
		return nil, err
	}

	return b, nil
}

// GetBid returns a bid by ID.
func (s *BidService) GetBid(ctx context.Context, id uuid.UUID) (*domain.Bid, error) {
	return s.bids.GetByID(ctx, id)
}

// ListBidsForListing returns all bids for a listing.
// IDOR guard: callerID must equal listing.OwnerUserID.
func (s *BidService) ListBidsForListing(ctx context.Context, listingID, callerID uuid.UUID) ([]*domain.Bid, error) {
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return nil, err
	}

	// Only listing owner may see all bids.
	if listing.OwnerUserID != callerID {
		return nil, domain.ErrListingNotFound // 404 to avoid enumeration
	}

	return s.bids.ListByListing(ctx, listingID)
}

// ListMyBids returns all bids placed by the caller (self-scoped).
func (s *BidService) ListMyBids(ctx context.Context, bidderUserID uuid.UUID) ([]*domain.Bid, error) {
	return s.bids.ListByBidder(ctx, bidderUserID)
}

// AcceptBid atomically accepts a bid, closes the listing, rejects sibling bids,
// and records the award. Event publish happens after the transaction commits.
// IDOR guard: callerID must equal listing.OwnerUserID.
//
// TOCTOU protection: all status checks are performed INSIDE the transaction on
// SELECT ... FOR UPDATE locked rows. A second concurrent AcceptBid call blocks on
// the row lock and then observes the already-AWARDED listing status, returning
// ErrListingNotOpen (mapped to 409 Conflict at the handler layer).
func (s *BidService) AcceptBid(ctx context.Context, bidID, callerID uuid.UUID) (*domain.Award, error) {
	// Pre-flight: load bid outside the tx to obtain listing_id for the IDOR check.
	// This is a non-authoritative read — all invariants are re-checked under row locks
	// inside the transaction below.
	preflight, err := s.bids.GetByID(ctx, bidID)
	if err != nil {
		return nil, err
	}

	listingID := preflight.ListingID

	var award *domain.Award

	// Atomic transaction: lock both rows, re-check invariants, then mutate.
	err = s.txManager.WithTx(ctx, func(txCtx context.Context, txListings store.ListingStore, txBids store.BidStore, txAwards store.AwardStore) error {
		// 1. Lock bid row first (lower PK in lock-ordering convention → bid then listing
		//    avoids deadlock when paired with listing-first paths; both rows always locked
		//    in the same order here since AcceptBid is the only path that locks both).
		lockedBid, lockBidErr := txBids.GetByIDForUpdate(txCtx, bidID)
		if lockBidErr != nil {
			return lockBidErr
		}

		// Re-check bid invariants on the locked row.
		if lockedBid.Status != domain.BidStatusPending {
			return domain.ErrBidNotPending
		}

		// 2. Lock listing row.
		lockedListing, lockListingErr := txListings.GetByIDForUpdate(txCtx, listingID)
		if lockListingErr != nil {
			return lockListingErr
		}

		// Re-check listing invariants on the locked row.
		if lockedListing.OwnerUserID != callerID {
			return domain.ErrListingNotFound // 404 to avoid enumeration
		}

		if lockedListing.Status != domain.ListingStatusOpen {
			return domain.ErrListingNotOpen
		}

		// Confirm the bid still belongs to the locked listing (referential sanity).
		if lockedBid.ListingID != lockedListing.ID {
			return domain.ErrBidNotFound
		}

		now := time.Now().UTC()

		// 3. Update bid to ACCEPTED.
		lockedBid.Status = domain.BidStatusAccepted
		lockedBid.DecidedAt = &now

		if updateErr := txBids.Update(txCtx, lockedBid); updateErr != nil {
			return fmt.Errorf("accept bid: %w", updateErr)
		}

		// 4. Flip listing to AWARDED.
		bidIDCopy := lockedBid.ID
		lockedListing.Status = domain.ListingStatusAwarded
		lockedListing.AwardedBidID = &bidIDCopy

		if updateErr := txListings.Update(txCtx, lockedListing); updateErr != nil {
			return fmt.Errorf("award listing: %w", updateErr)
		}

		// 5. Reject all other PENDING bids on this listing.
		if rejectErr := txBids.RejectSiblingBids(txCtx, lockedListing.ID, lockedBid.ID); rejectErr != nil {
			return fmt.Errorf("reject siblings: %w", rejectErr)
		}

		// 6. Insert the authoritative award record.
		award = &domain.Award{
			ID:           uuid.New(),
			ListingID:    lockedListing.ID,
			BidID:        lockedBid.ID,
			OwnerUserID:  lockedListing.OwnerUserID,
			BidderUserID: lockedBid.BidderUserID,
			Amount:       lockedBid.Amount,
			Currency:     lockedBid.Currency,
			CreatedAt:    now,
		}

		if insertErr := txAwards.Create(txCtx, award); insertErr != nil {
			return fmt.Errorf("create award: %w", insertErr)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// After commit: best-effort publish marketplace.bid_accepted.
	// Event publish uses a detached context so a client disconnect does not
	// suppress the publish attempt (backend-security §5 / goroutine MUST NOT inherit
	// request context — DB write must survive client disconnect).
	go func() { //nolint:contextcheck,gosec // G118+contextcheck: detached context per backend-security §5; goroutine must not be canceled on client disconnect
		publishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		evt := buildBidAcceptedEvent(award)

		if pubErr := s.publisher.PublishBidAccepted(publishCtx, &evt); pubErr != nil {
			slog.Warn(
				"bid_accepted publish failed; award row is authoritative",
				"award_id", award.ID,
				"err", pubErr,
			)

			return
		}

		// Mark event_published_at in DB — best-effort, non-fatal on failure.
		markCtx, markCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer markCancel()

		if markErr := s.awards.MarkEventPublished(markCtx, award.ID); markErr != nil {
			slog.Warn(
				"mark award event_published_at failed",
				"award_id", award.ID,
				"err", markErr,
			)
		}
	}()

	return award, nil
}

// RejectBid rejects a PENDING bid.
// IDOR guard: callerID must equal listing.OwnerUserID.
func (s *BidService) RejectBid(ctx context.Context, bidID, callerID uuid.UUID) (*domain.Bid, error) {
	bid, err := s.bids.GetByID(ctx, bidID)
	if err != nil {
		return nil, err
	}

	if bid.Status != domain.BidStatusPending {
		return nil, domain.ErrBidNotPending
	}

	// IDOR: load listing to verify ownership.
	listing, err := s.listings.GetByID(ctx, bid.ListingID)
	if err != nil {
		return nil, err
	}

	if listing.OwnerUserID != callerID {
		return nil, domain.ErrBidNotFound // 404 to avoid enumeration
	}

	now := time.Now().UTC()
	bid.Status = domain.BidStatusRejected
	bid.DecidedAt = &now

	if err := s.bids.Update(ctx, bid); err != nil {
		return nil, err
	}

	return bid, nil
}

// WithdrawBid withdraws a PENDING bid.
// IDOR guard: callerID must equal bid.BidderUserID.
func (s *BidService) WithdrawBid(ctx context.Context, bidID, callerID uuid.UUID) (*domain.Bid, error) {
	bid, err := s.bids.GetByID(ctx, bidID)
	if err != nil {
		return nil, err
	}

	if bid.Status != domain.BidStatusPending {
		return nil, domain.ErrBidNotPending
	}

	// IDOR: bidder can only withdraw their own bid.
	if bid.BidderUserID != callerID {
		return nil, domain.ErrBidNotFound // 404 to avoid enumeration
	}

	now := time.Now().UTC()
	bid.Status = domain.BidStatusWithdrawn
	bid.DecidedAt = &now

	if err := s.bids.Update(ctx, bid); err != nil {
		return nil, err
	}

	return bid, nil
}

// buildBidAcceptedEvent constructs the marketplace.bid_accepted event payload.
func buildBidAcceptedEvent(award *domain.Award) domain.BidAcceptedEvent {
	return domain.BidAcceptedEvent{
		EventID:    uuid.New().String(),
		OccurredAt: time.Now().UTC(),
		Version:    1,
		Data: domain.BidAcceptedData{
			AwardID:      award.ID,
			ListingID:    award.ListingID,
			BidID:        award.BidID,
			OwnerUserID:  award.OwnerUserID,
			BidderUserID: award.BidderUserID,
			Amount:       award.Amount.String(), // string to preserve numeric precision
			Currency:     award.Currency,
		},
	}
}
