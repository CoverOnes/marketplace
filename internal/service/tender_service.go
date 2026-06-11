package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/client"
	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/events"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
)

// TenderService handles tender business logic: role CRUD, collaborator apply/accept/exit,
// and milestone CRUD. Owner-only governance is enforced here (not at the router layer)
// so callerID checks are always on the locked listing row.
type TenderService struct {
	listings        store.ListingStore
	roles           store.TenderRoleStore
	collaborators   store.TenderCollaboratorStore
	milestones      store.TenderMilestoneStore
	tenderTx        store.TenderTxManager
	workspaceClient client.WorkspaceClient // nil = workspace call skipped (dev/test)
	publisher       events.Publisher
}

// NewTenderService returns a TenderService.
// workspaceClient may be nil; when nil the add-party call is skipped (local dev).
// publisher must not be nil; pass events.NewNoopPublisher() when Redis is absent.
func NewTenderService(
	listings store.ListingStore,
	roles store.TenderRoleStore,
	collaborators store.TenderCollaboratorStore,
	milestones store.TenderMilestoneStore,
	tenderTx store.TenderTxManager,
	workspaceClient client.WorkspaceClient,
	publisher events.Publisher,
) *TenderService {
	return &TenderService{
		listings:        listings,
		roles:           roles,
		collaborators:   collaborators,
		milestones:      milestones,
		tenderTx:        tenderTx,
		workspaceClient: workspaceClient,
		publisher:       publisher,
	}
}

// --- Role CRUD ---

// CreateRoleInput carries validated input for creating a tender role.
type CreateRoleInput struct {
	ListingID        uuid.UUID
	CallerID         uuid.UUID // must equal listing.OwnerUserID
	Title            string
	Description      string
	MaxCollaborators *int
	ProfitShareBPS   *int
	ProfitShareNote  string
	SortOrder        int
}

// CreateRole creates a new role under a tender listing.
// Owner-only. Listing must be a tender with tender_status = OPEN or PARTIALLY_STAFFED.
// Terminal tenders (SETTLING, COMPLETED, CANCELED) reject new roles.
func (s *TenderService) CreateRole(ctx context.Context, in *CreateRoleInput) (*domain.TenderRole, error) {
	if err := validateRoleInput(in.Title, in.Description, in.MaxCollaborators, in.ProfitShareBPS); err != nil {
		return nil, err
	}

	listing, err := s.listings.GetByID(ctx, in.ListingID)
	if err != nil {
		return nil, err
	}

	if !listing.IsTender {
		return nil, domain.ErrNotTenderListing
	}

	if listing.OwnerUserID != in.CallerID {
		return nil, domain.ErrListingNotFound // 404 to avoid enumeration
	}

	if !isTenderAcceptingStructuralChanges(listing.TenderStatus) {
		return nil, fmt.Errorf("%w: tender is not accepting structural changes in its current state", domain.ErrInvalidTenderTransition)
	}

	now := time.Now().UTC()
	r := &domain.TenderRole{
		ID:               uuid.New(),
		ListingID:        in.ListingID,
		Title:            in.Title,
		Description:      in.Description,
		MaxCollaborators: in.MaxCollaborators,
		ProfitShareBPS:   in.ProfitShareBPS,
		ProfitShareNote:  in.ProfitShareNote,
		Status:           domain.TenderRoleStatusOpen,
		SortOrder:        in.SortOrder,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := s.roles.Create(ctx, r); err != nil {
		return nil, fmt.Errorf("create tender role: %w", err)
	}

	return r, nil
}

// ListRoles returns all roles for a tender listing.
// Owner-only access.
func (s *TenderService) ListRoles(ctx context.Context, listingID, callerID uuid.UUID) ([]*domain.TenderRole, error) {
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return nil, err
	}

	if !listing.IsTender {
		return nil, domain.ErrNotTenderListing
	}

	if listing.OwnerUserID != callerID {
		return nil, domain.ErrListingNotFound // 404 to avoid enumeration
	}

	return s.roles.ListByListing(ctx, listingID)
}

// CloseRoleInput carries input for closing a tender role.
type CloseRoleInput struct {
	RoleID   uuid.UUID
	CallerID uuid.UUID // must equal listing.OwnerUserID
}

// CloseRole sets a tender role to CLOSED (owner-only).
// Terminal tenders (SETTLING, COMPLETED, CANCELED) reject role changes.
func (s *TenderService) CloseRole(ctx context.Context, in *CloseRoleInput) (*domain.TenderRole, error) {
	role, err := s.roles.GetByID(ctx, in.RoleID)
	if err != nil {
		return nil, err
	}

	listing, err := s.listings.GetByID(ctx, role.ListingID)
	if err != nil {
		return nil, err
	}

	// Fix 4: guard against non-tender listings (every other tender method asserts IsTender).
	if !listing.IsTender {
		return nil, domain.ErrNotTenderListing
	}

	if listing.OwnerUserID != in.CallerID {
		return nil, domain.ErrTenderRoleNotFound // 404 to avoid enumeration
	}

	if !isTenderAcceptingStructuralChanges(listing.TenderStatus) {
		return nil, fmt.Errorf("%w: tender is not accepting structural changes in its current state", domain.ErrInvalidTenderTransition)
	}

	if role.Status == domain.TenderRoleStatusClosed {
		return role, nil // idempotent
	}

	role.Status = domain.TenderRoleStatusClosed

	if err := s.roles.Update(ctx, role); err != nil {
		return nil, fmt.Errorf("close tender role: %w", err)
	}

	return role, nil
}

// --- Collaborator operations ---

// ApplyToRoleInput carries input for a vendor applying to a tender role.
type ApplyToRoleInput struct {
	RoleID       uuid.UUID
	VendorUserID uuid.UUID
	KYCTier      int // caller's current KYC tier from X-Kyc-Tier header
	JoinMessage  string
}

// ApplyToRole creates a PENDING collaborator record for a vendor applying to a role.
// KYC tier must be >= listing.kyc_tier_required.
// Phase 4: OPEN recruiter mode and join-while-EXECUTING are both enabled.
func (s *TenderService) ApplyToRole(ctx context.Context, in *ApplyToRoleInput) (*domain.TenderCollaborator, error) {
	if err := sanitizeMessage(in.JoinMessage); err != nil {
		return nil, fmt.Errorf("%w: join_message: %s", domain.ErrValidation, err)
	}

	role, err := s.roles.GetByID(ctx, in.RoleID)
	if err != nil {
		return nil, err
	}

	if role.Status != domain.TenderRoleStatusOpen {
		return nil, fmt.Errorf("%w: role is not open for applications", domain.ErrValidation)
	}

	listing, err := s.listings.GetByID(ctx, role.ListingID)
	if err != nil {
		return nil, err
	}

	if !listing.IsTender {
		return nil, domain.ErrNotTenderListing
	}

	// Reject applications to tenders that are not actively recruiting.
	// Phase 4 accepts OPEN, PARTIALLY_STAFFED, and EXECUTING.
	// SETTLING/COMPLETED/CANCELED never accept new applicants.
	if !isTenderAcceptingApplications(listing.TenderStatus) {
		return nil, fmt.Errorf("%w: tender is not accepting applications", domain.ErrValidation)
	}

	// KYC tier gate (mirrors RequireTier middleware for the listing-level requirement).
	if in.KYCTier < listing.KYCTierRequired {
		return nil, domain.ErrKYCTierRequired
	}

	// Vendor must not be the listing owner.
	if listing.OwnerUserID == in.VendorUserID {
		return nil, fmt.Errorf("%w: owner cannot apply as collaborator", domain.ErrValidation)
	}

	now := time.Now().UTC()
	c := &domain.TenderCollaborator{
		ID:           uuid.New(),
		TenderRoleID: in.RoleID,
		ListingID:    role.ListingID,
		VendorUserID: in.VendorUserID,
		Status:       domain.CollaboratorStatusPending,
		JoinMessage:  in.JoinMessage,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.collaborators.Create(ctx, c); err != nil {
		return nil, err
	}

	return c, nil
}

// AcceptCollaboratorInput carries input for accepting a collaborator application.
type AcceptCollaboratorInput struct {
	CollaboratorID uuid.UUID
	CallerID       uuid.UUID // must equal listing.OwnerUserID
}

// AcceptCollaborator atomically approves a PENDING collaborator application.
// Uses SELECT FOR UPDATE on the role row + count-APPROVED inside the same tx
// to prevent TOCTOU races on max_collaborators.
// If max_collaborators is reached after this accept, the role is set to FILLED.
//
// Phase 4: when the tender is in EXECUTING status, a synchronous S2S call to the
// workspace service is made after the tx commits to add the new party to the multiparty
// contract at 0 bps placeholder (Model B). Failure returns 502; the collaborator row
// stays APPROVED and P5 outbox will reconcile.
//
// TODO Phase 2: auto-advance tender_status (OPEN→PARTIALLY_STAFFED when first
// collaborator is approved; PARTIALLY_STAFFED→EXECUTING when all roles filled).
// ValidTenderTransition is defined and tested but NOT yet wired here.
func (s *TenderService) AcceptCollaborator(ctx context.Context, in *AcceptCollaboratorInput) (*domain.TenderCollaborator, error) {
	// Pre-flight: non-authoritative read to get IDs before locking.
	preflight, err := s.collaborators.GetByID(ctx, in.CollaboratorID)
	if err != nil {
		return nil, err
	}

	var result *domain.TenderCollaborator

	// tenderStatus is captured from the locked listing INSIDE the transaction via closure.
	var capturedTenderStatus domain.TenderStatus

	txErr := s.tenderTx.WithTenderTx(ctx, func(
		txCtx context.Context,
		txListings store.ListingStore,
		txRoles store.TenderRoleStore,
		txCollaborators store.TenderCollaboratorStore,
	) error {
		var innerErr error
		result, capturedTenderStatus, innerErr = s.acceptCollaboratorTx(
			txCtx, in, preflight.TenderRoleID, txListings, txRoles, txCollaborators,
		)

		return innerErr
	})
	if txErr != nil {
		return result, txErr
	}

	// Post-tx: if tender is EXECUTING, synchronously call workspace to add the party.
	// Failure is surfaces as 502 — collaborator stays APPROVED; P5 outbox reconciles.
	if capturedTenderStatus == domain.TenderStatusExecuting {
		if wsErr := s.addPartyToWorkspace(ctx, result, in.CallerID, capturedTenderStatus); wsErr != nil {
			return result, wsErr
		}
	}

	return result, nil
}

// addPartyToWorkspace calls workspace AddPartyToContract for a collaborator accepted
// while the tender is EXECUTING. Returns ErrUpstreamWorkspace on failure.
func (s *TenderService) addPartyToWorkspace(
	ctx context.Context,
	collab *domain.TenderCollaborator,
	posterUserID uuid.UUID,
	tenderStatus domain.TenderStatus,
) error {
	if s.workspaceClient == nil {
		slog.Warn(
			"workspace client not configured; skipping add-party call",
			"collaborator_id", collab.ID,
			"tender_id", collab.ListingID,
			"tender_status", tenderStatus,
		)

		return nil
	}

	roleID := collab.TenderRoleID // non-nil; always set on tender collaborators

	if err := s.workspaceClient.AddPartyToContract(ctx, client.AddPartyInput{
		TenderID:     collab.ListingID,
		VendorUserID: collab.VendorUserID,
		RoleID:       &roleID,
		ShareBps:     0,
		Currency:     nil,
		PosterUserID: &posterUserID,
	}); err != nil {
		slog.Error(
			"workspace add-party failed; collaborator is APPROVED but contract not updated — P5 outbox will reconcile",
			"collaborator_id", collab.ID,
			"tender_id", collab.ListingID,
			"err", err,
		)

		return fmt.Errorf("%w: %s", domain.ErrUpstreamWorkspace, err)
	}

	// §14 event: best-effort detached goroutine; only when accept happened on EXECUTING.
	s.publishCollaboratorJoinedAsync(ctx, collab, tenderStatus)

	return nil
}

// publishCollaboratorJoinedAsync publishes marketplace.collaborator_joined in a detached
// goroutine after the tx commits and workspace call succeeds. Best-effort: failures are
// logged but never propagated.
// The ctx parameter is accepted only for contextcheck call-chain analysis; the detached
// goroutine deliberately uses context.Background() (backend-security §5).
func (s *TenderService) publishCollaboratorJoinedAsync(_ context.Context, collab *domain.TenderCollaborator, tenderStatus domain.TenderStatus) {
	go func() { //nolint:contextcheck,gosec // G118+contextcheck: detached context per backend-security §5; goroutine must not be canceled on client disconnect
		publishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		roleID := collab.TenderRoleID

		evt := domain.CollaboratorJoinedEvent{
			EventID:    uuid.New().String(),
			OccurredAt: time.Now().UTC(),
			Version:    1,
			Data: domain.CollaboratorJoinedData{
				TenderID:       collab.ListingID,
				CollaboratorID: collab.ID,
				VendorUserID:   collab.VendorUserID,
				RoleID:         &roleID,
				TenderStatus:   string(tenderStatus),
			},
		}

		if pubErr := s.publisher.PublishCollaboratorJoined(publishCtx, &evt); pubErr != nil {
			slog.Warn(
				"collaborator_joined publish failed; collaborator row is authoritative",
				"collaborator_id", collab.ID,
				"tender_id", collab.ListingID,
				"err", pubErr,
			)
		}
	}()
}

// acceptCollaboratorTx is the transactional body of AcceptCollaborator.
// Extracted to keep AcceptCollaborator under the gocyclo limit (15).
// Returns the approved collaborator, the locked listing's TenderStatus (for post-tx
// workspace call), and any error.
func (s *TenderService) acceptCollaboratorTx(
	ctx context.Context,
	in *AcceptCollaboratorInput,
	roleID uuid.UUID,
	txListings store.ListingStore,
	txRoles store.TenderRoleStore,
	txCollaborators store.TenderCollaboratorStore,
) (*domain.TenderCollaborator, domain.TenderStatus, error) {
	// 1. Lock the role row first (prevents concurrent accepts on the same role).
	lockedRole, err := txRoles.GetByIDForUpdate(ctx, roleID)
	if err != nil {
		return nil, "", err
	}

	// 2. Lock the listing row to verify caller ownership under the lock.
	lockedListing, err := txListings.GetByIDForUpdate(ctx, lockedRole.ListingID)
	if err != nil {
		return nil, "", err
	}

	if lockedListing.OwnerUserID != in.CallerID {
		return nil, "", domain.ErrTenderCollaboratorNotFound // 404 to avoid enumeration
	}

	// Capture the tender status from inside the lock (authoritative; avoids TOCTOU
	// between the pre-tx read and the post-tx workspace call).
	var lockedTenderStatus domain.TenderStatus
	if lockedListing.TenderStatus != nil {
		lockedTenderStatus = *lockedListing.TenderStatus
	}

	// 2a. Reject accepts on terminal tender states (SETTLING / COMPLETED / CANCELED).
	// SETTLING is terminal: the tender is winding down and no new collaborators should
	// be accepted. Omitting it lets an owner approve on a SETTLING tender, producing
	// inconsistent DB state (an APPROVED collaborator on a closing tender).
	if lockedTenderStatus == domain.TenderStatusSettling ||
		lockedTenderStatus == domain.TenderStatusCompleted ||
		lockedTenderStatus == domain.TenderStatusCancelled {
		return nil, lockedTenderStatus, domain.ErrInvalidTenderTransition
	}

	// 3. Lock collaborator row.
	lockedCollab, err := txCollaborators.GetByIDForUpdate(ctx, in.CollaboratorID)
	if err != nil {
		return nil, lockedTenderStatus, err
	}

	// Idempotency: if the collaborator is already APPROVED (e.g. a previous call
	// succeeded but the workspace S2S call failed and the client retried), return
	// the current row as success. This lets the caller re-trigger the workspace
	// add-party call without hitting ErrValidation/400 on the re-try.
	if lockedCollab.Status == domain.CollaboratorStatusApproved {
		return lockedCollab, lockedTenderStatus, nil
	}

	if lockedCollab.Status != domain.CollaboratorStatusPending {
		return nil, lockedTenderStatus, fmt.Errorf("%w: collaborator is not pending", domain.ErrValidation)
	}

	// 4. Check max_collaborators INSIDE the transaction to prevent TOCTOU.
	if lockedRole.MaxCollaborators != nil {
		approved, countErr := txCollaborators.CountApprovedByRole(ctx, lockedRole.ID)
		if countErr != nil {
			return nil, lockedTenderStatus, fmt.Errorf("count approved: %w", countErr)
		}

		if approved >= *lockedRole.MaxCollaborators {
			result, overflowErr := s.rejectOverflowCollaborator(ctx, lockedCollab, lockedRole, txCollaborators, txRoles)

			return result, lockedTenderStatus, overflowErr
		}
	}

	// 5. Approve the collaborator.
	result, approveErr := s.doApproveCollaborator(ctx, in.CallerID, lockedCollab, lockedRole, txCollaborators, txRoles)

	return result, lockedTenderStatus, approveErr
}

// rejectOverflowCollaborator rejects a collaborator because the role is at capacity,
// and marks the role FILLED. Called from within a transaction.
func (s *TenderService) rejectOverflowCollaborator(
	ctx context.Context,
	lockedCollab *domain.TenderCollaborator,
	lockedRole *domain.TenderRole,
	txCollaborators store.TenderCollaboratorStore,
	txRoles store.TenderRoleStore,
) (*domain.TenderCollaborator, error) {
	lockedCollab.Status = domain.CollaboratorStatusRejected
	lockedCollab.ApprovedAt = nil

	if updateErr := txCollaborators.Update(ctx, lockedCollab); updateErr != nil {
		return nil, fmt.Errorf("reject overflow collaborator: %w", updateErr)
	}

	lockedRole.Status = domain.TenderRoleStatusFilled

	if updateErr := txRoles.Update(ctx, lockedRole); updateErr != nil {
		return nil, fmt.Errorf("fill role: %w", updateErr)
	}

	return lockedCollab, domain.ErrTenderRoleFilled
}

// doApproveCollaborator approves a collaborator and fills the role if at capacity.
// Called from within a transaction.
func (s *TenderService) doApproveCollaborator(
	ctx context.Context,
	callerID uuid.UUID,
	lockedCollab *domain.TenderCollaborator,
	lockedRole *domain.TenderRole,
	txCollaborators store.TenderCollaboratorStore,
	txRoles store.TenderRoleStore,
) (*domain.TenderCollaborator, error) {
	now := time.Now().UTC()
	lockedCollab.Status = domain.CollaboratorStatusApproved
	lockedCollab.ApprovedAt = &now
	approvedBy := callerID
	lockedCollab.ApprovedByUserID = &approvedBy

	if updateErr := txCollaborators.Update(ctx, lockedCollab); updateErr != nil {
		return nil, fmt.Errorf("approve collaborator: %w", updateErr)
	}

	// Check if role is now filled after this accept.
	if lockedRole.MaxCollaborators != nil {
		approved, countErr := txCollaborators.CountApprovedByRole(ctx, lockedRole.ID)
		if countErr != nil {
			return nil, fmt.Errorf("count approved after accept: %w", countErr)
		}

		if approved >= *lockedRole.MaxCollaborators {
			lockedRole.Status = domain.TenderRoleStatusFilled

			if updateErr := txRoles.Update(ctx, lockedRole); updateErr != nil {
				return nil, fmt.Errorf("fill role after accept: %w", updateErr)
			}
		}
	}

	return lockedCollab, nil
}

// RejectCollaboratorInput carries input for rejecting a collaborator application.
type RejectCollaboratorInput struct {
	CollaboratorID uuid.UUID
	CallerID       uuid.UUID // must equal listing.OwnerUserID
}

// RejectCollaborator atomically rejects a PENDING collaborator application.
//
// Fix 1 (TOCTOU): all reads and the update run inside a single transaction with
// SELECT ... FOR UPDATE locks on both the collaborator and listing rows.
// A concurrent AcceptCollaborator that already promoted the row to APPROVED will
// be visible here (the locked read sees the committed APPROVED status), and we
// return ErrConflict instead of silently downgrading the row back to REJECTED.
func (s *TenderService) RejectCollaborator(ctx context.Context, in *RejectCollaboratorInput) (*domain.TenderCollaborator, error) {
	// Pre-flight: non-authoritative read to get the role_id before locking.
	preflight, err := s.collaborators.GetByID(ctx, in.CollaboratorID)
	if err != nil {
		return nil, err
	}

	var result *domain.TenderCollaborator

	txErr := s.tenderTx.WithTenderTx(ctx, func(
		txCtx context.Context,
		txListings store.ListingStore,
		txRoles store.TenderRoleStore,
		txCollaborators store.TenderCollaboratorStore,
	) error {
		// 1. Lock the role row (establishes lock-ordering with AcceptCollaborator).
		lockedRole, lockRoleErr := txRoles.GetByIDForUpdate(txCtx, preflight.TenderRoleID)
		if lockRoleErr != nil {
			return lockRoleErr
		}

		// 2. Lock the listing row to verify ownership under the lock.
		lockedListing, lockListingErr := txListings.GetByIDForUpdate(txCtx, lockedRole.ListingID)
		if lockListingErr != nil {
			return lockListingErr
		}

		if lockedListing.OwnerUserID != in.CallerID {
			return domain.ErrTenderCollaboratorNotFound // 404 to avoid enumeration
		}

		// 3. Lock the collaborator row and re-check status under the lock.
		lockedCollab, lockCollabErr := txCollaborators.GetByIDForUpdate(txCtx, in.CollaboratorID)
		if lockCollabErr != nil {
			return lockCollabErr
		}

		// If the row is no longer PENDING (e.g. concurrently APPROVED), do NOT
		// overwrite it. Return a conflict error so the caller knows the state changed.
		if lockedCollab.Status != domain.CollaboratorStatusPending {
			result = lockedCollab

			return fmt.Errorf("%w: collaborator is no longer pending (current status: %s)",
				domain.ErrConflict, lockedCollab.Status)
		}

		lockedCollab.Status = domain.CollaboratorStatusRejected

		if updateErr := txCollaborators.Update(txCtx, lockedCollab); updateErr != nil {
			return fmt.Errorf("reject collaborator: %w", updateErr)
		}

		result = lockedCollab

		return nil
	})
	if txErr != nil {
		return result, txErr
	}

	return result, nil
}

// ExitCollaboratorInput carries input for a vendor exiting a role.
type ExitCollaboratorInput struct {
	CollaboratorID uuid.UUID
	CallerID       uuid.UUID // must equal collaborator.VendorUserID
	Reason         string
}

// ExitCollaborator allows a vendor to exit a role.
// PENDING → WITHDRAWN; APPROVED → EXITED.
//
// Fix (TOCTOU): all reads and the update run inside a single transaction with
// SELECT ... FOR UPDATE locks on the collaborator row (and the role row for
// lock-ordering consistency with AcceptCollaborator). This prevents a race
// where a concurrent AcceptCollaborator promotes PENDING→APPROVED between our
// read and update, causing the accept to be silently overwritten with WITHDRAWN.
//
// TODO Phase 2: after an APPROVED vendor exits, check if the role should
// revert from FILLED→OPEN, and whether tender_status should step back
// (e.g. EXECUTING→PARTIALLY_STAFFED). ValidTenderTransition covers these
// edges but they are not wired in Phase 1.
func (s *TenderService) ExitCollaborator(ctx context.Context, in *ExitCollaboratorInput) (*domain.TenderCollaborator, error) {
	if err := sanitizeMessage(in.Reason); err != nil {
		return nil, fmt.Errorf("%w: reason: %s", domain.ErrValidation, err)
	}

	// Pre-flight: non-authoritative read to get role_id for lock-ordering.
	preflight, err := s.collaborators.GetByID(ctx, in.CollaboratorID)
	if err != nil {
		return nil, err
	}

	var result *domain.TenderCollaborator

	txErr := s.tenderTx.WithTenderTx(ctx, func(
		txCtx context.Context,
		txListings store.ListingStore,
		txRoles store.TenderRoleStore,
		txCollaborators store.TenderCollaboratorStore,
	) error {
		// 1. Lock the role row first (maintains lock-ordering with AcceptCollaborator /
		//    RejectCollaborator: role → listing → collaborator, preventing deadlocks).
		lockedRole, lockRoleErr := txRoles.GetByIDForUpdate(txCtx, preflight.TenderRoleID)
		if lockRoleErr != nil {
			return lockRoleErr
		}

		// 2. Lock the listing row to maintain the role→listing→collaborator lock-ordering
		// invariant that AcceptCollaborator and RejectCollaborator both hold.
		// Without this lock, ExitCollaborator skips the listing step, breaking the ordering
		// guarantee and risking deadlocks when paired with concurrent accept/reject paths.
		// The result is intentionally discarded — this lock is acquired for ordering only.
		if _, lockListingErr := txListings.GetByIDForUpdate(txCtx, lockedRole.ListingID); lockListingErr != nil {
			return lockListingErr
		}

		// 2. Lock the collaborator row and re-read status under the lock.
		lockedCollab, lockCollabErr := txCollaborators.GetByIDForUpdate(txCtx, in.CollaboratorID)
		if lockCollabErr != nil {
			return lockCollabErr
		}

		// IDOR: only the vendor themselves can exit (checked on the locked row).
		if lockedCollab.VendorUserID != in.CallerID {
			return domain.ErrTenderCollaboratorNotFound // 404 to avoid enumeration
		}

		now := time.Now().UTC()

		switch lockedCollab.Status {
		case domain.CollaboratorStatusPending:
			lockedCollab.Status = domain.CollaboratorStatusWithdrawn
			lockedCollab.ExitReason = in.Reason
			lockedCollab.ExitedAt = &now

		case domain.CollaboratorStatusApproved:
			lockedCollab.Status = domain.CollaboratorStatusExited
			lockedCollab.ExitReason = in.Reason
			lockedCollab.ExitedAt = &now

		default:
			return fmt.Errorf("%w: collaborator is not in an active state", domain.ErrValidation)
		}

		if updateErr := txCollaborators.Update(txCtx, lockedCollab); updateErr != nil {
			return fmt.Errorf("exit collaborator: %w", updateErr)
		}

		result = lockedCollab

		return nil
	})
	if txErr != nil {
		return result, txErr
	}

	return result, nil
}

// ListCollaborators returns all collaborators for a tender listing (owner-only).
func (s *TenderService) ListCollaborators(ctx context.Context, listingID, callerID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return nil, err
	}

	if !listing.IsTender {
		return nil, domain.ErrNotTenderListing
	}

	if listing.OwnerUserID != callerID {
		return nil, domain.ErrListingNotFound // 404 to avoid enumeration
	}

	return s.collaborators.ListByListing(ctx, listingID)
}

// --- Milestone CRUD ---

// CreateMilestoneInput carries validated input for creating a tender milestone.
type CreateMilestoneInput struct {
	ListingID uuid.UUID
	CallerID  uuid.UUID // must equal listing.OwnerUserID
	Title     string
	DueDate   *time.Time
	Amount    *string // decimal string, nil = no fixed amount
	Currency  *string
}

// CreateMilestone creates a new milestone for a tender listing (owner-only).
// Terminal tenders (SETTLING, COMPLETED, CANCELED) reject new milestones.
func (s *TenderService) CreateMilestone(ctx context.Context, in *CreateMilestoneInput) (*domain.TenderMilestone, error) {
	if err := validateMilestoneInput(in.Title, in.Amount, in.Currency); err != nil {
		return nil, err
	}

	listing, err := s.listings.GetByID(ctx, in.ListingID)
	if err != nil {
		return nil, err
	}

	if !listing.IsTender {
		return nil, domain.ErrNotTenderListing
	}

	if listing.OwnerUserID != in.CallerID {
		return nil, domain.ErrListingNotFound // 404 to avoid enumeration
	}

	if !isTenderAcceptingStructuralChanges(listing.TenderStatus) {
		return nil, fmt.Errorf("%w: tender is not accepting structural changes in its current state", domain.ErrInvalidTenderTransition)
	}

	now := time.Now().UTC()
	m := &domain.TenderMilestone{
		ID:        uuid.New(),
		ListingID: in.ListingID,
		Title:     in.Title,
		DueDate:   in.DueDate,
		Status:    domain.MilestoneStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if in.Amount != nil {
		d, parseErr := parseDecimal(*in.Amount)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: amount: %s", domain.ErrValidation, parseErr)
		}

		m.Amount = d
	}

	if in.Currency != nil {
		m.Currency = in.Currency
	}

	if err := s.milestones.Create(ctx, m); err != nil {
		return nil, fmt.Errorf("create tender milestone: %w", err)
	}

	return m, nil
}

// ListMilestones returns all milestones for a tender listing (owner-only).
func (s *TenderService) ListMilestones(ctx context.Context, listingID, callerID uuid.UUID) ([]*domain.TenderMilestone, error) {
	listing, err := s.listings.GetByID(ctx, listingID)
	if err != nil {
		return nil, err
	}

	if !listing.IsTender {
		return nil, domain.ErrNotTenderListing
	}

	if listing.OwnerUserID != callerID {
		return nil, domain.ErrListingNotFound // 404 to avoid enumeration
	}

	return s.milestones.ListByListing(ctx, listingID)
}

// --- Validation helpers ---

const (
	maxRoleTitleRunes       = 200
	maxRoleDescriptionRunes = 5000
	maxMilestoneTitleRunes  = 200
	maxBPS                  = 10000
)

// isTenderAcceptingStructuralChanges returns true when a tender's current status
// permits owner-driven structural mutations: adding/closing roles, adding milestones.
// OPEN and PARTIALLY_STAFFED allow structural changes.
// SETTLING, COMPLETED, and CANCELED are terminal — reject mutations.
// EXECUTING is intentionally excluded: the tender is live and its structure is frozen.
// nil tender_status means the listing is CLASSIC (not a tender) — return false.
func isTenderAcceptingStructuralChanges(ts *domain.TenderStatus) bool {
	if ts == nil {
		return false
	}

	switch *ts {
	case domain.TenderStatusOpen, domain.TenderStatusPartiallyStaffed:
		return true
	}

	return false
}

// isTenderAcceptingApplications returns true when a tender's current status
// permits new PENDING collaborator applications.
// Phase 4: OPEN, PARTIALLY_STAFFED, and EXECUTING all accept new applicants.
// SETTLING, COMPLETED, and CANCELED never accept new applicants.
// nil tender_status means the listing is CLASSIC (not a tender) — return false.
func isTenderAcceptingApplications(ts *domain.TenderStatus) bool {
	if ts == nil {
		return false
	}

	switch *ts {
	case domain.TenderStatusOpen, domain.TenderStatusPartiallyStaffed, domain.TenderStatusExecuting:
		return true
	}

	return false
}

func validateRoleInput(title, description string, maxCollaborators, profitShareBPS *int) error {
	if err := sanitizeText(title); err != nil {
		return fmt.Errorf("%w: title: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(title) < 1 || utf8.RuneCountInString(title) > maxRoleTitleRunes {
		return fmt.Errorf("%w: title must be 1-%d characters", domain.ErrValidation, maxRoleTitleRunes)
	}

	if err := sanitizeText(description); err != nil {
		return fmt.Errorf("%w: description: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(description) > maxRoleDescriptionRunes {
		return fmt.Errorf("%w: description exceeds %d characters", domain.ErrValidation, maxRoleDescriptionRunes)
	}

	if maxCollaborators != nil && *maxCollaborators <= 0 {
		return fmt.Errorf("%w: max_collaborators must be > 0 when set", domain.ErrValidation)
	}

	if profitShareBPS != nil && (*profitShareBPS < 0 || *profitShareBPS > maxBPS) {
		return fmt.Errorf("%w: profit_share_bps must be 0..10000", domain.ErrValidation)
	}

	return nil
}

func validateMilestoneInput(title string, amount, currency *string) error {
	if err := sanitizeText(title); err != nil {
		return fmt.Errorf("%w: title: %s", domain.ErrValidation, err)
	}

	if utf8.RuneCountInString(title) < 1 || utf8.RuneCountInString(title) > maxMilestoneTitleRunes {
		return fmt.Errorf("%w: title must be 1-%d characters", domain.ErrValidation, maxMilestoneTitleRunes)
	}

	if (amount != nil) != (currency != nil) {
		return fmt.Errorf("%w: amount and currency must both be set or both omitted", domain.ErrValidation)
	}

	if currency != nil && len(*currency) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter code", domain.ErrValidation)
	}

	// Fix 3: reject amounts that exceed the numeric(14,2) column maximum to prevent
	// DB overflow → 500 instead of 400 (mirrors validateBudget in listing_service.go).
	if amount != nil {
		d, parseErr := parseDecimal(*amount)
		if parseErr != nil {
			return fmt.Errorf("%w: amount: %s", domain.ErrValidation, parseErr)
		}

		if d.GreaterThan(maxNumeric14_2) {
			return fmt.Errorf("%w: amount exceeds maximum allowed value", domain.ErrValidation)
		}
	}

	return nil
}
