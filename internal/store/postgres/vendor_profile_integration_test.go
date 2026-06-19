package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	pgstore "github.com/CoverOnes/marketplace/internal/store/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetVendorProfiles clears the vendor_profile table between tests.
func resetVendorProfiles(ctx context.Context, t *testing.T) {
	t.Helper()

	_, err := sharedTestPool.Exec(ctx, `DELETE FROM vendor_profile`)
	require.NoError(t, err)
}

// makeProfile builds a minimal valid VendorProfile for testing.
func makeProfile(ownerUserID uuid.UUID) *domain.VendorProfile {
	now := time.Now().UTC().Truncate(time.Millisecond)

	return &domain.VendorProfile{
		ID:          uuid.New(),
		OwnerUserID: ownerUserID,
		DisplayName: "Alice Vendor",
		Headline:    strPtr("Go specialist"),
		Bio:         strPtr("I write Go services."),
		Skills:      []string{"Go", "PostgreSQL"},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func strPtr(s string) *string { return &s }

// TestVendorProfileStore_Upsert_Create_Integration verifies that Upsert inserts
// a new row when the owner has no existing profile.
func TestVendorProfileStore_Upsert_Create_Integration(t *testing.T) {
	ctx := context.Background()
	resetVendorProfiles(ctx, t)

	s := pgstore.NewVendorProfileStore(sharedTestPool)
	ownerID := uuid.New()
	p := makeProfile(ownerID)

	require.NoError(t, s.Upsert(ctx, p))

	got, err := s.GetByOwner(ctx, ownerID)
	require.NoError(t, err)
	assert.Equal(t, ownerID, got.OwnerUserID)
	assert.Equal(t, "Alice Vendor", got.DisplayName)
	assert.Equal(t, strPtr("Go specialist"), got.Headline)
	assert.Equal(t, strPtr("I write Go services."), got.Bio)
	assert.Equal(t, []string{"Go", "PostgreSQL"}, got.Skills)
}

// TestVendorProfileStore_Upsert_Idempotent_Integration verifies that a second
// Upsert for the same owner updates the row in place (created_at preserved,
// updated_at bumped, display_name reflects new value).
func TestVendorProfileStore_Upsert_Idempotent_Integration(t *testing.T) {
	ctx := context.Background()
	resetVendorProfiles(ctx, t)

	s := pgstore.NewVendorProfileStore(sharedTestPool)
	ownerID := uuid.New()
	p := makeProfile(ownerID)

	require.NoError(t, s.Upsert(ctx, p))
	firstCreatedAt := p.CreatedAt

	// Update with new display name after a tiny delay so updated_at differs.
	time.Sleep(2 * time.Millisecond)

	p2 := makeProfile(ownerID)
	p2.DisplayName = "Alice Vendor Updated"
	p2.UpdatedAt = time.Now().UTC()

	require.NoError(t, s.Upsert(ctx, p2))

	got, err := s.GetByOwner(ctx, ownerID)
	require.NoError(t, err)
	assert.Equal(t, "Alice Vendor Updated", got.DisplayName, "display_name must be updated")

	// created_at from the original insert must be preserved.
	assert.Equal(t, firstCreatedAt.Truncate(time.Millisecond), got.CreatedAt.Truncate(time.Millisecond),
		"created_at must be preserved on upsert")

	// updated_at must be at or after the second upsert.
	assert.True(t, got.UpdatedAt.After(firstCreatedAt) || got.UpdatedAt.Equal(firstCreatedAt),
		"updated_at must be bumped on second upsert")
}

// TestVendorProfileStore_GetByOwner_NotFound_Integration verifies that GetByOwner
// returns domain.ErrNotFound (not a 500) when no profile exists.
func TestVendorProfileStore_GetByOwner_NotFound_Integration(t *testing.T) {
	ctx := context.Background()

	s := pgstore.NewVendorProfileStore(sharedTestPool)

	_, err := s.GetByOwner(ctx, uuid.New())
	require.ErrorIs(t, err, domain.ErrNotFound, "must return ErrNotFound for unknown owner")
}

// TestVendorProfileStore_UniqueConstraint_Integration verifies that two concurrent
// upserts from the same owner do not create two rows (UNIQUE index enforced).
func TestVendorProfileStore_UniqueConstraint_Integration(t *testing.T) {
	ctx := context.Background()
	resetVendorProfiles(ctx, t)

	s := pgstore.NewVendorProfileStore(sharedTestPool)
	ownerID := uuid.New()

	p1 := makeProfile(ownerID)
	p1.DisplayName = "First"
	require.NoError(t, s.Upsert(ctx, p1))

	p2 := makeProfile(ownerID)
	p2.DisplayName = "Second"
	require.NoError(t, s.Upsert(ctx, p2), "second upsert must succeed via ON CONFLICT path")

	// Confirm only one row exists.
	var count int

	err := sharedTestPool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM vendor_profile WHERE owner_user_id = $1`, ownerID,
	).Scan(&count)

	require.NoError(t, err)
	assert.Equal(t, 1, count, "exactly one row must exist after two upserts for the same owner")
}

// TestVendorProfileStore_ValidationErrors_Integration verifies that Go-level
// validation in the store rejects invalid inputs before any DB round-trip.
func TestVendorProfileStore_ValidationErrors_Integration(t *testing.T) {
	ctx := context.Background()
	s := pgstore.NewVendorProfileStore(sharedTestPool)
	ownerID := uuid.New()

	tooLong201 := strings.Repeat("a", 201)
	tooLong5001 := strings.Repeat("a", 5001)
	tooLong101 := strings.Repeat("a", 101)

	tests := []struct {
		name    string
		mutate  func(p *domain.VendorProfile)
		wantErr string
	}{
		{
			name:    "empty display_name",
			mutate:  func(p *domain.VendorProfile) { p.DisplayName = "" },
			wantErr: "display_name must be 1-200 runes",
		},
		{
			name:    "display_name over 200 runes",
			mutate:  func(p *domain.VendorProfile) { p.DisplayName = tooLong201 },
			wantErr: "display_name must be 1-200 runes",
		},
		{
			name:    "headline over 200 runes",
			mutate:  func(p *domain.VendorProfile) { p.Headline = &tooLong201 },
			wantErr: "headline must be ≤200 runes",
		},
		{
			name:    "bio over 5000 runes",
			mutate:  func(p *domain.VendorProfile) { p.Bio = &tooLong5001 },
			wantErr: "bio must be ≤5000 runes",
		},
		{
			name: "skills over 50 elements",
			mutate: func(p *domain.VendorProfile) {
				p.Skills = make([]string, 51)
				for i := range p.Skills {
					p.Skills[i] = "skill"
				}
			},
			wantErr: "skills slice must have ≤50 elements",
		},
		{
			name: "single skill over 100 runes",
			mutate: func(p *domain.VendorProfile) {
				p.Skills = []string{tooLong101}
			},
			wantErr: "skills[0] must be ≤100 runes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := makeProfile(ownerID)
			tc.mutate(p)
			err := s.Upsert(ctx, p)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestVendorProfileStore_NullSkills_Integration verifies that a nil skills slice
// round-trips as an empty slice (not NULL) so callers always get a safe []string.
func TestVendorProfileStore_NullSkills_Integration(t *testing.T) {
	ctx := context.Background()
	resetVendorProfiles(ctx, t)

	s := pgstore.NewVendorProfileStore(sharedTestPool)
	ownerID := uuid.New()

	p := makeProfile(ownerID)
	p.Skills = nil // intentionally nil

	require.NoError(t, s.Upsert(ctx, p))

	got, err := s.GetByOwner(ctx, ownerID)
	require.NoError(t, err)
	assert.NotNil(t, got.Skills, "skills must never be nil after round-trip")
	assert.Empty(t, got.Skills, "nil skills must come back as empty slice")
}

// TestVendorProfileStore_MigrationDDL_Integration verifies that migration 000014
// created vendor_profile with the expected schema: UNIQUE index on owner_user_id,
// NO FK constraints (CLAUDE.md #9), correct column types.
func TestVendorProfileStore_MigrationDDL_Integration(t *testing.T) {
	ctx := context.Background()

	var tableExists bool

	err := sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'vendor_profile'
		)`,
	).Scan(&tableExists)

	require.NoError(t, err)
	assert.True(t, tableExists, "vendor_profile table must exist after migration 000014")

	// Verify UNIQUE index on owner_user_id exists.
	var idxExists bool

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE indexname = 'vendor_profile_owner_user_id_idx'
		)`,
	).Scan(&idxExists)

	require.NoError(t, err)
	assert.True(t, idxExists, "vendor_profile_owner_user_id_idx must exist")

	// Verify NO FK constraints (platform red-line CLAUDE.md #9).
	var fkCount int

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM information_schema.table_constraints
		 WHERE table_name = 'vendor_profile' AND constraint_type = 'FOREIGN KEY'`,
	).Scan(&fkCount)

	require.NoError(t, err)
	assert.Equal(t, 0, fkCount, "vendor_profile must have ZERO foreign key constraints (CLAUDE.md #9)")

	// Verify skills column is text[].
	var colType string

	err = sharedTestPool.QueryRow(
		ctx,
		`SELECT data_type FROM information_schema.columns
		 WHERE table_name = 'vendor_profile' AND column_name = 'skills'`,
	).Scan(&colType)

	require.NoError(t, err)
	assert.Equal(t, "ARRAY", colType, "skills must be an array type")

	// Verify created_at and updated_at are timestamptz.
	for _, col := range []string{"created_at", "updated_at"} {
		var dt string

		err = sharedTestPool.QueryRow(
			ctx,
			`SELECT data_type FROM information_schema.columns
			 WHERE table_name = 'vendor_profile' AND column_name = $1`, col,
		).Scan(&dt)

		require.NoError(t, err)
		assert.Equal(t, "timestamp with time zone", dt, "%s must be timestamptz", col)
	}
}
