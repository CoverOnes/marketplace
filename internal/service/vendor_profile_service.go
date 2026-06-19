package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/outbox"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
)

// Field length limits for vendor profile (backend-security §5.2).
// MUST mirror the DB CHECK constraints in migration 000014 and the constants
// in store/postgres/vendor_profile_store.go.
const (
	vpMaxDisplayNameRunes = 200
	vpMaxHeadlineRunes    = 200
	vpMaxBioRunes         = 5000
	vpMaxSkillRunes       = 100
	vpMaxSkillsCount      = 50
)

// VendorProfileService handles vendor profile business logic.
//
// V2: pass a non-nil vendorProfileOutbox to enable same-tx outbox enqueue on
// Upsert when embeddable text changes (auto-embedding pipeline). When nil, Upsert
// operates as a plain store call (V1 / test stubs that bypass the tx manager).
type VendorProfileService struct {
	profiles            store.VendorProfileStore
	vendorProfileOutbox store.VendorProfileOutboxTxManager
}

// NewVendorProfileService returns a VendorProfileService.
// vendorProfileOutbox MUST be nil when no embedding pipeline is desired (test
// stubs or V1 callers); pass a real VendorProfileOutboxTxManager to enable V2
// same-tx outbox enqueue on create/update.
func NewVendorProfileService(
	profiles store.VendorProfileStore,
	vendorProfileOutbox store.VendorProfileOutboxTxManager,
) *VendorProfileService {
	return &VendorProfileService{
		profiles:            profiles,
		vendorProfileOutbox: vendorProfileOutbox,
	}
}

// UpsertVendorProfileInput carries validated input for upserting a vendor profile.
// OwnerUserID MUST be set exclusively from the identity context — never from the
// request body.
type UpsertVendorProfileInput struct {
	OwnerUserID uuid.UUID
	DisplayName string
	Headline    *string
	Bio         *string
	Skills      []string
}

// validateUpsertInput sanitizes and length-validates all text fields in UpsertVendorProfileInput
// (backend-security §5.2 + §5.4). Runs in the service layer so handler tests
// using stub stores exercise the same rules as the real postgres store.
func validateUpsertInput(in UpsertVendorProfileInput) error {
	dnLen := utf8.RuneCountInString(in.DisplayName)
	if dnLen < 1 || dnLen > vpMaxDisplayNameRunes {
		return fmt.Errorf("%w: display_name must be 1-%d runes (got %d)",
			domain.ErrValidation, vpMaxDisplayNameRunes, dnLen)
	}

	if err := sanitizeText(in.DisplayName); err != nil {
		return fmt.Errorf("%w: display_name %s", domain.ErrValidation, err)
	}

	if err := validateOptionalSingleLine("headline", in.Headline, vpMaxHeadlineRunes); err != nil {
		return err
	}

	if err := validateOptionalMultiline("bio", in.Bio, vpMaxBioRunes); err != nil {
		return err
	}

	return validateSkills(in.Skills)
}

func validateOptionalSingleLine(field string, v *string, maxRunes int) error {
	if v == nil {
		return nil
	}

	n := utf8.RuneCountInString(*v)
	if n > maxRunes {
		return fmt.Errorf("%w: %s must be ≤%d runes (got %d)", domain.ErrValidation, field, maxRunes, n)
	}

	if err := sanitizeText(*v); err != nil {
		return fmt.Errorf("%w: %s %s", domain.ErrValidation, field, err)
	}

	return nil
}

func validateOptionalMultiline(field string, v *string, maxRunes int) error {
	if v == nil {
		return nil
	}

	n := utf8.RuneCountInString(*v)
	if n > maxRunes {
		return fmt.Errorf("%w: %s must be ≤%d runes (got %d)", domain.ErrValidation, field, maxRunes, n)
	}

	// Bio permits newlines (textarea); use sanitizeMultilineText.
	if err := sanitizeMultilineText(*v); err != nil {
		return fmt.Errorf("%w: %s %s", domain.ErrValidation, field, err)
	}

	return nil
}

func validateSkills(skills []string) error {
	if len(skills) > vpMaxSkillsCount {
		return fmt.Errorf("%w: skills slice must have ≤%d elements (got %d)",
			domain.ErrValidation, vpMaxSkillsCount, len(skills))
	}

	for i, sk := range skills {
		skLen := utf8.RuneCountInString(sk)
		// V1 Minor #1: reject empty-string skill entries (e.g. ["Go", ""] → 400).
		if skLen == 0 {
			return fmt.Errorf("%w: skills[%d] must not be empty", domain.ErrValidation, i)
		}

		if skLen > vpMaxSkillRunes {
			return fmt.Errorf("%w: skills[%d] must be ≤%d runes (got %d)",
				domain.ErrValidation, i, vpMaxSkillRunes, skLen)
		}

		if err := sanitizeText(sk); err != nil {
			return fmt.Errorf("%w: skills[%d] %s", domain.ErrValidation, i, err)
		}
	}

	return nil
}

// derefStr dereferences a string pointer, returning "" when nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// vendorTextChanged reports whether the embeddable text fields of a vendor profile
// differ between the current DB state (old) and the new values. Any change to
// displayName, headline, bio, or skills triggers a reindex.
func vendorTextChanged(old *domain.VendorProfile, newDisplayName string, newHeadline, newBio *string, newSkills []string) bool {
	if old.DisplayName != newDisplayName {
		return true
	}

	if derefStr(old.Headline) != derefStr(newHeadline) {
		return true
	}

	if derefStr(old.Bio) != derefStr(newBio) {
		return true
	}

	return strings.Join(old.Skills, ",") != strings.Join(newSkills, ",")
}

// Upsert creates or updates the vendor profile for the given owner.
// Sanitizes and length-validates all text fields (backend-security §5.2, §5.4)
// before delegating to the store layer.
//
// V2 outbox: when vendorProfileOutbox is non-nil, the profile write and the
// vendor_embedding_reindex enqueue happen in the SAME Postgres transaction.
// For a create (no existing profile) the event is always enqueued. For an update
// the event is enqueued only when the embeddable text changed (displayName,
// headline, bio, or skills) — a re-PUT with identical values does NOT enqueue.
func (s *VendorProfileService) Upsert(ctx context.Context, in UpsertVendorProfileInput) (*domain.VendorProfile, error) {
	if err := validateUpsertInput(in); err != nil {
		return nil, err
	}

	skills := in.Skills
	if skills == nil {
		skills = []string{}
	}

	now := time.Now().UTC()
	p := &domain.VendorProfile{
		ID:          uuid.New(),
		OwnerUserID: in.OwnerUserID,
		DisplayName: in.DisplayName,
		Headline:    in.Headline,
		Bio:         in.Bio,
		Skills:      skills,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if s.vendorProfileOutbox != nil {
		return s.upsertWithOutbox(ctx, in, p)
	}

	if err := s.profiles.Upsert(ctx, p); err != nil {
		return nil, err
	}

	return p, nil
}

// upsertWithOutbox performs the vendor_profile upsert and conditionally enqueues
// a vendor_embedding_reindex event in the SAME Postgres transaction.
// On create (ErrNotFound → no old row) the event is always enqueued.
// On update the event is enqueued only when the embeddable text changed.
func (s *VendorProfileService) upsertWithOutbox(
	ctx context.Context,
	in UpsertVendorProfileInput,
	p *domain.VendorProfile,
) (*domain.VendorProfile, error) {
	// Peek at the existing row BEFORE opening the transaction so we can snapshot
	// the old text for the textChanged check.  The record-not-found case is a
	// create, which always enqueues.
	old, lookupErr := s.profiles.GetByOwner(ctx, in.OwnerUserID)

	isCreate := errors.Is(lookupErr, domain.ErrNotFound)
	if lookupErr != nil && !isCreate {
		return nil, fmt.Errorf("lookup vendor_profile before upsert: %w", lookupErr)
	}

	skills := p.Skills // already normalised above

	shouldEnqueue := isCreate || vendorTextChanged(old, in.DisplayName, in.Headline, in.Bio, skills)

	var created *domain.VendorProfile

	err := s.vendorProfileOutbox.WithVendorProfileOutboxTx(ctx, func(
		txCtx context.Context,
		txProfiles store.VendorProfileStore,
		ob store.OutboxStore,
	) error {
		if upsertErr := txProfiles.Upsert(txCtx, p); upsertErr != nil {
			return fmt.Errorf("upsert vendor_profile: %w", upsertErr)
		}

		if shouldEnqueue {
			if enqErr := outbox.EnqueueVendorEmbeddingReindex(txCtx, ob, in.OwnerUserID); enqErr != nil {
				return fmt.Errorf("enqueue vendor_embedding_reindex: %w", enqErr)
			}
		}

		created = p

		return nil
	})
	if err != nil {
		return nil, err
	}

	return created, nil
}

// Get returns the vendor profile for the given owner user.
// Returns domain.ErrNotFound when the user has no profile (caller maps to 404).
func (s *VendorProfileService) Get(ctx context.Context, ownerUserID uuid.UUID) (*domain.VendorProfile, error) {
	p, err := s.profiles.GetByOwner(ctx, ownerUserID)
	if err != nil {
		return nil, err
	}

	return p, nil
}
