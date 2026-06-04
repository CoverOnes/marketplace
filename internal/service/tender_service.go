package service

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
)

// TenderService handles tender business logic: role CRUD, collaborator apply/accept/exit,
// and milestone CRUD. Owner-only governance is enforced here (not at the router layer)
// so callerID checks are always on the locked listing row.
type TenderService struct {
	listings      store.ListingStore
	roles         store.TenderRoleStore
	collaborators store.TenderCollaboratorStore
	milestones    store.TenderMilestoneStore
	tenderTx      store.TenderTxManager
}

// NewTenderService returns a TenderService.
func NewTenderService(
	listings store.ListingStore,
	roles store.TenderRoleStore,
	collaborators store.TenderCollaboratorStore,
	milestones store.TenderMilestoneStore,
	tenderTx store.TenderTxManager,
) *TenderService {
	return &TenderService{
		listings:      listings,
		roles:         roles,
		collaborators: collaborators,
		milestones:    milestones,
		tenderTx:      tenderTx,
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
func (s *TenderService) CloseRole(ctx context.Context, in *CloseRoleInput) (*domain.TenderRole, error) {
	role, err := s.roles.GetByID(ctx, in.RoleID)
	if err != nil {
		return nil, err
	}

	listing, err := s.listings.GetByID(ctx, role.ListingID)
	if err != nil {
		return nil, err
	}

	if listing.OwnerUserID != in.CallerID {
		return nil, domain.ErrTenderRoleNotFound // 404 to avoid enumeration
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
// OPEN recruiter mode is API-rejected in Phase 1.
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

	// Phase 1: reject OPEN recruiter mode.
	if listing.RecruiterMode == domain.RecruiterModeOpen {
		return nil, domain.ErrOpenRecruiterNotEnabled
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
func (s *TenderService) AcceptCollaborator(ctx context.Context, in *AcceptCollaboratorInput) (*domain.TenderCollaborator, error) {
	// Pre-flight: non-authoritative read to get IDs before locking.
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
		var innerErr error
		result, innerErr = s.acceptCollaboratorTx(txCtx, in, preflight.TenderRoleID, txListings, txRoles, txCollaborators)

		return innerErr
	})
	if txErr != nil {
		return result, txErr
	}

	return result, nil
}

// acceptCollaboratorTx is the transactional body of AcceptCollaborator.
// Extracted to keep AcceptCollaborator under the gocyclo limit (15).
func (s *TenderService) acceptCollaboratorTx(
	ctx context.Context,
	in *AcceptCollaboratorInput,
	roleID uuid.UUID,
	txListings store.ListingStore,
	txRoles store.TenderRoleStore,
	txCollaborators store.TenderCollaboratorStore,
) (*domain.TenderCollaborator, error) {
	// 1. Lock the role row first (prevents concurrent accepts on the same role).
	lockedRole, err := txRoles.GetByIDForUpdate(ctx, roleID)
	if err != nil {
		return nil, err
	}

	// 2. Lock the listing row to verify caller ownership under the lock.
	lockedListing, err := txListings.GetByIDForUpdate(ctx, lockedRole.ListingID)
	if err != nil {
		return nil, err
	}

	if lockedListing.OwnerUserID != in.CallerID {
		return nil, domain.ErrTenderCollaboratorNotFound // 404 to avoid enumeration
	}

	// 3. Lock collaborator row.
	lockedCollab, err := txCollaborators.GetByIDForUpdate(ctx, in.CollaboratorID)
	if err != nil {
		return nil, err
	}

	if lockedCollab.Status != domain.CollaboratorStatusPending {
		return nil, fmt.Errorf("%w: collaborator is not pending", domain.ErrValidation)
	}

	// 4. Check max_collaborators INSIDE the transaction to prevent TOCTOU.
	if lockedRole.MaxCollaborators != nil {
		approved, countErr := txCollaborators.CountApprovedByRole(ctx, lockedRole.ID)
		if countErr != nil {
			return nil, fmt.Errorf("count approved: %w", countErr)
		}

		if approved >= *lockedRole.MaxCollaborators {
			return s.rejectOverflowCollaborator(ctx, lockedCollab, lockedRole, txCollaborators, txRoles)
		}
	}

	// 5. Approve the collaborator.
	return s.doApproveCollaborator(ctx, in.CallerID, lockedCollab, lockedRole, txCollaborators, txRoles)
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

// RejectCollaborator rejects a PENDING collaborator application.
func (s *TenderService) RejectCollaborator(ctx context.Context, in *RejectCollaboratorInput) (*domain.TenderCollaborator, error) {
	collab, err := s.collaborators.GetByID(ctx, in.CollaboratorID)
	if err != nil {
		return nil, err
	}

	role, err := s.roles.GetByID(ctx, collab.TenderRoleID)
	if err != nil {
		return nil, err
	}

	listing, err := s.listings.GetByID(ctx, role.ListingID)
	if err != nil {
		return nil, err
	}

	if listing.OwnerUserID != in.CallerID {
		return nil, domain.ErrTenderCollaboratorNotFound // 404 to avoid enumeration
	}

	if collab.Status != domain.CollaboratorStatusPending {
		return nil, fmt.Errorf("%w: collaborator is not pending", domain.ErrValidation)
	}

	collab.Status = domain.CollaboratorStatusRejected

	if err := s.collaborators.Update(ctx, collab); err != nil {
		return nil, fmt.Errorf("reject collaborator: %w", err)
	}

	return collab, nil
}

// ExitCollaboratorInput carries input for a vendor exiting a role.
type ExitCollaboratorInput struct {
	CollaboratorID uuid.UUID
	CallerID       uuid.UUID // must equal collaborator.VendorUserID
	Reason         string
}

// ExitCollaborator allows a vendor to exit a role.
// PENDING → WITHDRAWN; APPROVED → EXITED.
func (s *TenderService) ExitCollaborator(ctx context.Context, in *ExitCollaboratorInput) (*domain.TenderCollaborator, error) {
	if err := sanitizeMessage(in.Reason); err != nil {
		return nil, fmt.Errorf("%w: reason: %s", domain.ErrValidation, err)
	}

	collab, err := s.collaborators.GetByID(ctx, in.CollaboratorID)
	if err != nil {
		return nil, err
	}

	// IDOR: only the vendor themselves can exit.
	if collab.VendorUserID != in.CallerID {
		return nil, domain.ErrTenderCollaboratorNotFound // 404 to avoid enumeration
	}

	now := time.Now().UTC()

	switch collab.Status {
	case domain.CollaboratorStatusPending:
		collab.Status = domain.CollaboratorStatusWithdrawn
		collab.ExitReason = in.Reason
		collab.ExitedAt = &now

	case domain.CollaboratorStatusApproved:
		collab.Status = domain.CollaboratorStatusExited
		collab.ExitReason = in.Reason
		collab.ExitedAt = &now

	default:
		return nil, fmt.Errorf("%w: collaborator is not in an active state", domain.ErrValidation)
	}

	if err := s.collaborators.Update(ctx, collab); err != nil {
		return nil, fmt.Errorf("exit collaborator: %w", err)
	}

	return collab, nil
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

	return nil
}
