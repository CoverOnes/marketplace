package postgres

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Go-level validation constants for vendor_profile fields.
// MUST mirror the DB CHECK constraints in migration 000014 (backend-security §5.2).
const (
	maxDisplayNameRunes = 200
	maxHeadlineRunes    = 200
	maxBioRunes         = 5000
	maxSkillRunes       = 100
	maxSkillsCount      = 50
)

// VendorProfileStore is a pool-backed vendor profile store.
// It satisfies store.VendorProfileStore.
type VendorProfileStore struct {
	q querier
}

// NewVendorProfileStore returns a VendorProfileStore backed by pool.
func NewVendorProfileStore(pool *pgxpool.Pool) *VendorProfileStore {
	return &VendorProfileStore{q: pool}
}

// compile-time interface check.
var _ store.VendorProfileStore = (*VendorProfileStore)(nil)

// Upsert inserts or updates the vendor profile.
// Go-level validation is performed before the DB round-trip (backend-security §5.2).
func (s *VendorProfileStore) Upsert(ctx context.Context, p *domain.VendorProfile) error {
	return upsertVendorProfile(ctx, s.q, p)
}

// GetByOwner returns the vendor profile for ownerUserID, or domain.ErrNotFound.
func (s *VendorProfileStore) GetByOwner(ctx context.Context, ownerUserID uuid.UUID) (*domain.VendorProfile, error) {
	return getVendorProfileByOwner(ctx, s.q, ownerUserID)
}

// txVendorProfileStore is a transaction-scoped VendorProfileStore.
// Used by VendorProfileOutboxTxManager to run vendor_profile upsert + outbox
// Enqueue in a single Postgres transaction.
type txVendorProfileStore struct {
	tx querier
}

func (s *txVendorProfileStore) Upsert(ctx context.Context, p *domain.VendorProfile) error {
	return upsertVendorProfile(ctx, s.tx, p)
}

func (s *txVendorProfileStore) GetByOwner(ctx context.Context, ownerUserID uuid.UUID) (*domain.VendorProfile, error) {
	return getVendorProfileByOwner(ctx, s.tx, ownerUserID)
}

// --- helpers ---

// storeSanitizeSingleLine rejects null bytes, CR, LF, and ASCII control chars in
// single-line text fields (backend-security §5.4). Mirrors the service-layer
// sanitizeText so the store is independently safe for direct calls (e.g. indexer).
func storeSanitizeSingleLine(s string) error {
	for _, r := range s {
		if r == '\x00' || r == '\r' || r == '\n' {
			return fmt.Errorf("contains illegal control characters (null, CR, LF)")
		}

		if r < 0x20 && r != '\t' {
			return fmt.Errorf("contains ASCII control characters")
		}
	}

	return nil
}

// storeSanitizeMultiline rejects null bytes and ASCII control chars (excluding
// TAB, LF, and CR) in multi-line text fields (bio). Standalone \r is intentionally
// accepted because textarea content may use CRLF line endings.
// Mirrors the service-layer sanitizeMultilineText (backend-security §5.4).
func storeSanitizeMultiline(s string) error {
	for _, r := range s {
		if r == '\x00' {
			return fmt.Errorf("contains null byte")
		}

		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return fmt.Errorf("contains ASCII control characters")
		}
	}

	return nil
}

// validateVendorProfile enforces Go-level field constraints before any DB round-trip.
// Mirrors the CHECK constraints in migration 000014 (backend-security §5.2).
// Also rejects control characters so the store is independently safe for callers
// that bypass the service layer (e.g. the V2 outbox indexer path).
func validateVendorProfile(p *domain.VendorProfile) error {
	dnLen := utf8.RuneCountInString(p.DisplayName)
	if dnLen < 1 || dnLen > maxDisplayNameRunes {
		return fmt.Errorf("upsert vendor_profile: %w: display_name must be 1-%d runes (got %d)",
			domain.ErrValidation, maxDisplayNameRunes, dnLen)
	}

	if err := storeSanitizeSingleLine(p.DisplayName); err != nil {
		return fmt.Errorf("upsert vendor_profile: %w: display_name %s", domain.ErrValidation, err)
	}

	if p.Headline != nil {
		hlLen := utf8.RuneCountInString(*p.Headline)
		if hlLen > maxHeadlineRunes {
			return fmt.Errorf("upsert vendor_profile: %w: headline must be ≤%d runes (got %d)",
				domain.ErrValidation, maxHeadlineRunes, hlLen)
		}

		if err := storeSanitizeSingleLine(*p.Headline); err != nil {
			return fmt.Errorf("upsert vendor_profile: %w: headline %s", domain.ErrValidation, err)
		}
	}

	if p.Bio != nil {
		bioLen := utf8.RuneCountInString(*p.Bio)
		if bioLen > maxBioRunes {
			return fmt.Errorf("upsert vendor_profile: %w: bio must be ≤%d runes (got %d)",
				domain.ErrValidation, maxBioRunes, bioLen)
		}

		// Bio is a multiline field (textarea); storeSanitizeMultiline accepts \r and \n.
		// Standalone \r (bare CR from CRLF textarea content) is intentionally accepted.
		if err := storeSanitizeMultiline(*p.Bio); err != nil {
			return fmt.Errorf("upsert vendor_profile: %w: bio %s", domain.ErrValidation, err)
		}
	}

	if len(p.Skills) > maxSkillsCount {
		return fmt.Errorf("upsert vendor_profile: %w: skills slice must have ≤%d elements (got %d)",
			domain.ErrValidation, maxSkillsCount, len(p.Skills))
	}

	for i, sk := range p.Skills {
		skLen := utf8.RuneCountInString(sk)
		if skLen > maxSkillRunes {
			return fmt.Errorf("upsert vendor_profile: %w: skills[%d] must be ≤%d runes (got %d)",
				domain.ErrValidation, i, maxSkillRunes, skLen)
		}

		if err := storeSanitizeSingleLine(sk); err != nil {
			return fmt.Errorf("upsert vendor_profile: %w: skills[%d] %s", domain.ErrValidation, i, err)
		}
	}

	return nil
}

func upsertVendorProfile(ctx context.Context, q querier, p *domain.VendorProfile) error {
	if err := validateVendorProfile(p); err != nil {
		return err
	}

	// Ensure Skills is never NULL in the DB; an empty Go slice becomes '{}'.
	skills := p.Skills
	if skills == nil {
		skills = []string{}
	}

	const query = `
INSERT INTO vendor_profile (id, owner_user_id, display_name, headline, bio, skills, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (owner_user_id) DO UPDATE
    SET display_name = EXCLUDED.display_name,
        headline     = EXCLUDED.headline,
        bio          = EXCLUDED.bio,
        skills       = EXCLUDED.skills,
        updated_at   = EXCLUDED.updated_at
RETURNING id, owner_user_id, display_name, headline, bio, skills, created_at, updated_at
`

	row := q.QueryRow(
		ctx, query,
		p.ID,
		p.OwnerUserID,
		p.DisplayName,
		p.Headline,
		p.Bio,
		skills,
		p.CreatedAt,
		p.UpdatedAt,
	)

	updated, err := scanVendorProfile(row)
	if err != nil {
		return fmt.Errorf("upsert vendor_profile: %w", err)
	}

	// Reflect the RETURNING values back into the caller's struct so created_at
	// (preserved by the conflict path) is always accurate.
	*p = *updated

	return nil
}

func getVendorProfileByOwner(ctx context.Context, q querier, ownerUserID uuid.UUID) (*domain.VendorProfile, error) {
	const query = `
SELECT id, owner_user_id, display_name, headline, bio, skills, created_at, updated_at
FROM vendor_profile
WHERE owner_user_id = $1
`

	row := q.QueryRow(ctx, query, ownerUserID)

	p, err := scanVendorProfile(row)
	if err != nil {
		return nil, fmt.Errorf("get vendor_profile by owner: %w", err)
	}

	return p, nil
}

func scanVendorProfile(row pgx.Row) (*domain.VendorProfile, error) {
	var p domain.VendorProfile

	err := row.Scan(
		&p.ID,
		&p.OwnerUserID,
		&p.DisplayName,
		&p.Headline,
		&p.Bio,
		&p.Skills,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}

		return nil, fmt.Errorf("scan vendor_profile: %w", err)
	}

	if p.Skills == nil {
		p.Skills = []string{}
	}

	return &p, nil
}
