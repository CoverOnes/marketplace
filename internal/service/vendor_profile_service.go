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
// V2 extension point: pass a non-nil outboxEnqueuer to enable same-tx outbox
// enqueue after Upsert (for auto-embedding). When nil, Upsert operates as a
// plain store call (V1 behavior).
type VendorProfileService struct {
	profiles store.VendorProfileStore
	// outboxEnqueuer is intentionally nil in V1.  V2 will inject a real
	// implementation that enqueues a vendor_profile_reindex event in the
	// same Postgres transaction as the upsert.
	outboxEnqueuer VendorProfileOutboxEnqueuer
}

// VendorProfileOutboxEnqueuer is the hook V2 will implement to enqueue a
// reindex event in the same transaction as the vendor_profile upsert.
// In V1 this is always nil; the service checks for nil before calling.
type VendorProfileOutboxEnqueuer interface {
	EnqueueReindex(ctx context.Context, ownerUserID uuid.UUID) error
}

// NewVendorProfileService returns a VendorProfileService.
// outboxEnqueuer MUST be nil in V1; V2 passes a real implementation.
func NewVendorProfileService(
	profiles store.VendorProfileStore,
	outboxEnqueuer VendorProfileOutboxEnqueuer,
) *VendorProfileService {
	return &VendorProfileService{
		profiles:       profiles,
		outboxEnqueuer: outboxEnqueuer,
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

// Upsert creates or updates the vendor profile for the given owner.
// Sanitizes and length-validates all text fields (backend-security §5.2, §5.4)
// before delegating to the store layer.
//
// V2 hook: when outboxEnqueuer is non-nil, EnqueueReindex is called after a
// successful upsert to schedule embedding regeneration.
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

	if err := s.profiles.Upsert(ctx, p); err != nil {
		return nil, err
	}

	// V2 hook — nil in V1.
	if s.outboxEnqueuer != nil {
		if err := s.outboxEnqueuer.EnqueueReindex(ctx, in.OwnerUserID); err != nil {
			return nil, fmt.Errorf("enqueue vendor_profile reindex: %w", err)
		}
	}

	return p, nil
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
