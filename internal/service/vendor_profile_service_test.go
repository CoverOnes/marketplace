package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub store ---

type stubVendorProfileStore struct {
	profiles map[uuid.UUID]*domain.VendorProfile
	upsertFn func(p *domain.VendorProfile) error
}

func newStubVendorProfileStore() *stubVendorProfileStore {
	return &stubVendorProfileStore{profiles: make(map[uuid.UUID]*domain.VendorProfile)}
}

func (s *stubVendorProfileStore) Upsert(_ context.Context, p *domain.VendorProfile) error {
	if s.upsertFn != nil {
		return s.upsertFn(p)
	}

	s.profiles[p.OwnerUserID] = p

	return nil
}

func (s *stubVendorProfileStore) GetByOwner(_ context.Context, ownerUserID uuid.UUID) (*domain.VendorProfile, error) {
	p, ok := s.profiles[ownerUserID]
	if !ok {
		return nil, domain.ErrNotFound
	}

	return p, nil
}

// --- helpers ---

func strPtrSvc(s string) *string { return &s }

func makeUpsertInput(ownerID uuid.UUID) service.UpsertVendorProfileInput {
	return service.UpsertVendorProfileInput{
		OwnerUserID: ownerID,
		DisplayName: "Alice Vendor",
		Headline:    strPtrSvc("Go specialist"),
		Bio:         strPtrSvc("I write Go services.\nMultiple lines are fine."),
		Skills:      []string{"Go", "PostgreSQL"},
	}
}

// TestVendorProfileService_Upsert_HappyPath verifies a successful upsert
// returns the profile with owner_user_id from input (never from body).
func TestVendorProfileService_Upsert_HappyPath(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	in := makeUpsertInput(ownerID)
	p, err := svc.Upsert(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, ownerID, p.OwnerUserID)
	assert.Equal(t, "Alice Vendor", p.DisplayName)
	assert.Equal(t, strPtrSvc("Go specialist"), p.Headline)
	assert.Equal(t, []string{"Go", "PostgreSQL"}, p.Skills)
}

// TestVendorProfileService_Upsert_NilSkillsBecomesEmpty verifies that nil skills
// becomes an empty slice in the stored profile.
func TestVendorProfileService_Upsert_NilSkillsBecomesEmpty(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	in := makeUpsertInput(ownerID)
	in.Skills = nil

	p, err := svc.Upsert(context.Background(), in)
	require.NoError(t, err)
	assert.NotNil(t, p.Skills)
	assert.Empty(t, p.Skills)
}

// TestVendorProfileService_Upsert_ValidationErrors verifies that the service
// rejects control characters and oversized fields before calling the store.
func TestVendorProfileService_Upsert_ValidationErrors(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()

	tests := []struct {
		name    string
		mutate  func(in *service.UpsertVendorProfileInput)
		wantErr error
	}{
		{
			name: "null byte in display_name",
			mutate: func(in *service.UpsertVendorProfileInput) {
				in.DisplayName = "bad\x00name"
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "CR in display_name",
			mutate: func(in *service.UpsertVendorProfileInput) {
				in.DisplayName = "bad\rname"
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "LF in display_name",
			mutate: func(in *service.UpsertVendorProfileInput) {
				in.DisplayName = "bad\nname"
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "control char in headline",
			mutate: func(in *service.UpsertVendorProfileInput) {
				in.Headline = strPtrSvc("bad\x01headline")
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "null byte in bio",
			mutate: func(in *service.UpsertVendorProfileInput) {
				in.Bio = strPtrSvc("bad\x00bio")
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "control char in skill",
			mutate: func(in *service.UpsertVendorProfileInput) {
				in.Skills = []string{"go\x01bad"}
			},
			wantErr: domain.ErrValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := newStubVendorProfileStore()
			svc := service.NewVendorProfileService(st, nil)

			in := makeUpsertInput(ownerID)
			tc.mutate(&in)

			_, err := svc.Upsert(context.Background(), in)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantErr), "expected %v, got %v", tc.wantErr, err)
		})
	}
}

// TestVendorProfileService_Upsert_BioAllowsNewlines verifies that bio (multiline
// field) allows newlines without rejection.
func TestVendorProfileService_Upsert_BioAllowsNewlines(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	in := makeUpsertInput(ownerID)
	in.Bio = strPtrSvc("Line one.\nLine two.\nLine three.")

	_, err := svc.Upsert(context.Background(), in)
	require.NoError(t, err, "bio with newlines must be accepted")
}

// TestVendorProfileService_Upsert_StoreError propagates store errors back to the caller.
func TestVendorProfileService_Upsert_StoreError(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	st.upsertFn = func(_ *domain.VendorProfile) error {
		return errors.New("db connection error")
	}
	svc := service.NewVendorProfileService(st, nil)

	_, err := svc.Upsert(context.Background(), makeUpsertInput(ownerID))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db connection error")
}

// TestVendorProfileService_Get_HappyPath verifies Get returns the stored profile.
func TestVendorProfileService_Get_HappyPath(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	// Pre-populate via Upsert.
	_, err := svc.Upsert(context.Background(), makeUpsertInput(ownerID))
	require.NoError(t, err)

	p, err := svc.Get(context.Background(), ownerID)
	require.NoError(t, err)
	assert.Equal(t, ownerID, p.OwnerUserID)
	assert.Equal(t, "Alice Vendor", p.DisplayName)
}

// TestVendorProfileService_Get_NotFound verifies Get returns ErrNotFound when
// no profile exists — so the handler can map it to 404.
func TestVendorProfileService_Get_NotFound(t *testing.T) {
	t.Parallel()

	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	_, err := svc.Get(context.Background(), uuid.New())
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestVendorProfileService_Upsert_EmptySkillRejected verifies that an empty string
// within the skills slice is rejected with ErrValidation. (V1 Minor #1)
//
// A skills slice such as ["Go", ""] — e.g. produced by a client splitting on commas
// without trimming — must return 400, not silently store an empty skill entry.
func TestVendorProfileService_Upsert_EmptySkillRejected(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	tests := []struct {
		name   string
		skills []string
	}{
		{
			name:   "single empty skill",
			skills: []string{""},
		},
		{
			name:   "mixed valid and empty skill",
			skills: []string{"Go", ""},
		},
		{
			name:   "empty skill at start",
			skills: []string{"", "PostgreSQL"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := makeUpsertInput(ownerID)
			in.Skills = tc.skills

			_, err := svc.Upsert(context.Background(), in)
			require.Error(t, err, "empty skill entry must be rejected")
			assert.True(t, errors.Is(err, domain.ErrValidation),
				"empty skill must yield ErrValidation, got: %v", err)
		})
	}
}

// TestVendorProfileService_Upsert_BioAcceptsBareCarriageReturn verifies that bio
// accepts a bare \r character (V1 Minor #3 documented behavior).
//
// Rationale: textarea inputs in some web clients (especially on Windows) may send
// bare \r (CR without LF) as part of their line-ending encoding.
// sanitizeMultilineText intentionally permits standalone \r — it only rejects \x00
// and other ASCII control chars below 0x20 (excluding \t, \n, and \r).
// This test documents and locks that intentional acceptance.
func TestVendorProfileService_Upsert_BioAcceptsBareCarriageReturn(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	in := makeUpsertInput(ownerID)
	// Bare \r in bio — accepted by sanitizeMultilineText (see vendor_profile_store.go
	// storeSanitizeMultiline: \r is in the explicit exception list alongside \t and \n).
	in.Bio = strPtrSvc("Line one.\rLine two.")

	_, err := svc.Upsert(context.Background(), in)
	require.NoError(t, err, "bio with bare \\r must be accepted (sanitizeMultilineText intentionally permits standalone CR)")
}

// TestVendorProfileService_Upsert_LargeValidPayload verifies that the maximum
// valid payload (display_name 200, headline 200, bio 5000, 50 skills each 100)
// is accepted without error.
func TestVendorProfileService_Upsert_LargeValidPayload(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	st := newStubVendorProfileStore()
	svc := service.NewVendorProfileService(st, nil)

	in := service.UpsertVendorProfileInput{
		OwnerUserID: ownerID,
		DisplayName: strings.Repeat("a", 200),
		Headline:    strPtrSvc(strings.Repeat("h", 200)),
		Bio:         strPtrSvc(strings.Repeat("b", 5000)),
		Skills:      make([]string, 50),
	}

	for i := range in.Skills {
		in.Skills[i] = strings.Repeat("s", 100)
	}

	_, err := svc.Upsert(context.Background(), in)
	require.NoError(t, err, "max-size valid payload must be accepted")
}
