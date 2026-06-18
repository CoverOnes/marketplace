package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CoverOnes/marketplace/internal/domain"
	"github.com/CoverOnes/marketplace/internal/service"
	"github.com/CoverOnes/marketplace/internal/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contentTypePDF is a test constant to satisfy goconst (string appears ≥ threshold times across test files).
const contentTypePDF = "application/pdf"

// --- Fakes ---

// fakeListingStore is a minimal in-memory stub for ListingStore.
type fakeListingStore struct {
	listings map[uuid.UUID]*domain.Listing
}

func newFakeListingStore(listings ...*domain.Listing) *fakeListingStore {
	s := &fakeListingStore{listings: make(map[uuid.UUID]*domain.Listing)}
	for _, l := range listings {
		s.listings[l.ID] = l
	}

	return s
}

func (f *fakeListingStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Listing, error) {
	if l, ok := f.listings[id]; ok {
		return l, nil
	}

	return nil, domain.ErrListingNotFound
}

func (f *fakeListingStore) Create(_ context.Context, _ *domain.Listing) error { return nil }
func (f *fakeListingStore) GetByIDForUpdate(_ context.Context, _ uuid.UUID) (*domain.Listing, error) {
	return nil, errors.New("not implemented in fake")
}

func (f *fakeListingStore) List(_ context.Context, _ store.ListingFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (f *fakeListingStore) Search(_ context.Context, _ store.SearchFilter) ([]*domain.Listing, error) {
	return nil, nil
}

func (f *fakeListingStore) Update(_ context.Context, _ *domain.Listing) error { return nil }

// fakeAttachmentStore is a minimal in-memory stub for ListingAttachmentStore.
type fakeAttachmentStore struct {
	attachments map[uuid.UUID]*domain.ListingAttachment
	createErr   error
}

func newFakeAttachmentStore() *fakeAttachmentStore {
	return &fakeAttachmentStore{attachments: make(map[uuid.UUID]*domain.ListingAttachment)}
}

func (f *fakeAttachmentStore) Create(_ context.Context, a *domain.ListingAttachment) error {
	if f.createErr != nil {
		return f.createErr
	}

	f.attachments[a.ID] = a

	return nil
}

func (f *fakeAttachmentStore) GetByID(_ context.Context, id uuid.UUID) (*domain.ListingAttachment, error) {
	if a, ok := f.attachments[id]; ok {
		return a, nil
	}

	return nil, domain.ErrAttachmentNotFound
}

func (f *fakeAttachmentStore) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.ListingAttachment, error) {
	var out []*domain.ListingAttachment

	for _, a := range f.attachments {
		if a.ListingID == listingID && a.DetachedAt == nil {
			out = append(out, a)
		}
	}

	return out, nil
}

func (f *fakeAttachmentStore) CountActiveByListing(_ context.Context, listingID uuid.UUID) (int, error) {
	count := 0

	for _, a := range f.attachments {
		if a.ListingID == listingID && a.DetachedAt == nil {
			count++
		}
	}

	return count, nil
}

func (f *fakeAttachmentStore) Detach(_ context.Context, id, detachedBy uuid.UUID) error {
	a, ok := f.attachments[id]
	if !ok || a.DetachedAt != nil {
		return domain.ErrAttachmentNotFound
	}

	now := time.Now().UTC()
	a.DetachedAt = &now
	a.DetachedBy = &detachedBy

	return nil
}

// fakeCollaboratorStore is a minimal in-memory stub for TenderCollaboratorStore.
type fakeCollaboratorStore struct {
	collabs []*domain.TenderCollaborator
}

func newFakeCollaboratorStore(collabs ...*domain.TenderCollaborator) *fakeCollaboratorStore {
	return &fakeCollaboratorStore{collabs: collabs}
}

func (f *fakeCollaboratorStore) ListByListing(_ context.Context, listingID uuid.UUID) ([]*domain.TenderCollaborator, error) {
	var out []*domain.TenderCollaborator

	for _, c := range f.collabs {
		if c.ListingID == listingID {
			out = append(out, c)
		}
	}

	return out, nil
}

func (f *fakeCollaboratorStore) Create(_ context.Context, _ *domain.TenderCollaborator) error {
	return nil
}

func (f *fakeCollaboratorStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.TenderCollaborator, error) {
	return nil, nil
}

func (f *fakeCollaboratorStore) GetByIDForUpdate(_ context.Context, _ uuid.UUID) (*domain.TenderCollaborator, error) {
	return nil, nil
}

func (f *fakeCollaboratorStore) CountApprovedByRole(_ context.Context, _ uuid.UUID) (int, error) {
	return 0, nil
}

func (f *fakeCollaboratorStore) ListByRole(_ context.Context, _ uuid.UUID) ([]*domain.TenderCollaborator, error) {
	return nil, nil
}

func (f *fakeCollaboratorStore) Update(_ context.Context, _ *domain.TenderCollaborator) error {
	return nil
}

// fakeFileClient is an in-memory stub for FileClient.
type fakeFileClient struct {
	registerErr error
	presignURL  string
	presignErr  error
}

func (f *fakeFileClient) RegisterAttachment(_ context.Context, _, _, _ uuid.UUID) error {
	return f.registerErr
}

func (f *fakeFileClient) PresignAttachment(_ context.Context, _, _ uuid.UUID) (string, error) {
	return f.presignURL, f.presignErr
}

// --- Test helpers ---

func makeOpenListing(ownerID uuid.UUID) *domain.Listing {
	return &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusOpen,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
}

func makeApprovedCollab(listingID, vendorID uuid.UUID) *domain.TenderCollaborator {
	return &domain.TenderCollaborator{
		ID:           uuid.New(),
		ListingID:    listingID,
		VendorUserID: vendorID,
		Status:       domain.CollaboratorStatusApproved,
	}
}

func validAttachInput() service.AttachInput {
	return service.AttachInput{
		FileID:      uuid.New(),
		Filename:    "document.pdf",
		ContentType: contentTypePDF,
		SizeBytes:   1024,
	}
}

// newAttachmentService builds an AttachmentService with the supplied dependencies.
func newAttachmentService(
	attachStore *fakeAttachmentStore,
	listingStore *fakeListingStore,
	collabStore *fakeCollaboratorStore,
	fileClient *fakeFileClient,
) *service.AttachmentService {
	return service.NewAttachmentService(attachStore, listingStore, collabStore, fileClient)
}

// --- Attach tests ---

func TestAttachmentService_Attach_OwnerCanAttach(t *testing.T) {
	ownerID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	listingStore := newFakeListingStore(listing)
	collabStore := newFakeCollaboratorStore()
	fileClient := &fakeFileClient{presignURL: "https://example.com/download"}

	svc := newAttachmentService(attachStore, listingStore, collabStore, fileClient)

	a, err := svc.Attach(context.Background(), listing.ID, ownerID, validAttachInput())

	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, listing.ID, a.ListingID)
	assert.Equal(t, ownerID, a.UploaderID)
}

func TestAttachmentService_Attach_ApprovedCollaboratorCanAttach(t *testing.T) {
	ownerID := uuid.New()
	vendorID := uuid.New()
	listing := makeOpenListing(ownerID)
	collab := makeApprovedCollab(listing.ID, vendorID)

	attachStore := newFakeAttachmentStore()
	listingStore := newFakeListingStore(listing)
	collabStore := newFakeCollaboratorStore(collab)
	fileClient := &fakeFileClient{}

	svc := newAttachmentService(attachStore, listingStore, collabStore, fileClient)

	a, err := svc.Attach(context.Background(), listing.ID, vendorID, validAttachInput())

	require.NoError(t, err)
	assert.Equal(t, vendorID, a.UploaderID)
}

func TestAttachmentService_Attach_StrangerForbidden(t *testing.T) {
	ownerID := uuid.New()
	strangerID := uuid.New()
	listing := makeOpenListing(ownerID)

	svc := newAttachmentService(
		newFakeAttachmentStore(),
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	_, err := svc.Attach(context.Background(), listing.ID, strangerID, validAttachInput())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrAttachmentForbidden))
}

func TestAttachmentService_Attach_PendingCollaboratorForbidden(t *testing.T) {
	ownerID := uuid.New()
	vendorID := uuid.New()
	listing := makeOpenListing(ownerID)

	// Collaborator exists but is PENDING, not APPROVED.
	pendingCollab := &domain.TenderCollaborator{
		ID:           uuid.New(),
		ListingID:    listing.ID,
		VendorUserID: vendorID,
		Status:       domain.CollaboratorStatusPending,
	}

	svc := newAttachmentService(
		newFakeAttachmentStore(),
		newFakeListingStore(listing),
		newFakeCollaboratorStore(pendingCollab),
		&fakeFileClient{},
	)

	_, err := svc.Attach(context.Background(), listing.ID, vendorID, validAttachInput())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrAttachmentForbidden))
}

func TestAttachmentService_Attach_ListingNotFound(t *testing.T) {
	svc := newAttachmentService(
		newFakeAttachmentStore(),
		newFakeListingStore(), // empty
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	_, err := svc.Attach(context.Background(), uuid.New(), uuid.New(), validAttachInput())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrListingNotFound))
}

func TestAttachmentService_Attach_DisallowedMimeType(t *testing.T) {
	ownerID := uuid.New()
	listing := makeOpenListing(ownerID)

	svc := newAttachmentService(
		newFakeAttachmentStore(),
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	in := validAttachInput()
	in.ContentType = "application/x-malicious"

	_, err := svc.Attach(context.Background(), listing.ID, ownerID, in)

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrContentTypeNotAllowed))
}

func TestAttachmentService_Attach_CapReachedAt10(t *testing.T) {
	ownerID := uuid.New()
	listing := makeOpenListing(ownerID)
	attachStore := newFakeAttachmentStore()

	// Pre-populate 10 attachments.
	for i := 0; i < 10; i++ {
		a := &domain.ListingAttachment{
			ID:        uuid.New(),
			ListingID: listing.ID,
			CreatedAt: time.Now().UTC(),
		}
		attachStore.attachments[a.ID] = a
	}

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	_, err := svc.Attach(context.Background(), listing.ID, ownerID, validAttachInput())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrAttachmentCapReached))
}

func TestAttachmentService_Attach_FileClientRegisterError(t *testing.T) {
	ownerID := uuid.New()
	listing := makeOpenListing(ownerID)

	fileClient := &fakeFileClient{registerErr: domain.ErrUpstreamFile}

	svc := newAttachmentService(
		newFakeAttachmentStore(),
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		fileClient,
	)

	_, err := svc.Attach(context.Background(), listing.ID, ownerID, validAttachInput())

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}

// --- List tests ---

func TestAttachmentService_List_OpenListingPubliclyReadable(t *testing.T) {
	ownerID := uuid.New()
	strangerID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	// Pre-seed one attachment.
	a := &domain.ListingAttachment{
		ID:        uuid.New(),
		ListingID: listing.ID,
		CreatedAt: time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	// Stranger can list attachments on an OPEN listing.
	attachments, err := svc.List(context.Background(), listing.ID, strangerID)

	require.NoError(t, err)
	assert.Len(t, attachments, 1)
}

func TestAttachmentService_List_ClosedListingForbidsStranger(t *testing.T) {
	ownerID := uuid.New()
	strangerID := uuid.New()
	listing := &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusClosed,
	}

	svc := newAttachmentService(
		newFakeAttachmentStore(),
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	_, err := svc.List(context.Background(), listing.ID, strangerID)

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrAttachmentForbidden))
}

func TestAttachmentService_List_ReturnsOnlyActive(t *testing.T) {
	ownerID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()

	// Active attachment.
	active := &domain.ListingAttachment{
		ID:        uuid.New(),
		ListingID: listing.ID,
		CreatedAt: time.Now().UTC(),
	}
	attachStore.attachments[active.ID] = active

	// Detached attachment.
	detachTime := time.Now().UTC()
	detached := &domain.ListingAttachment{
		ID:         uuid.New(),
		ListingID:  listing.ID,
		DetachedAt: &detachTime,
		CreatedAt:  time.Now().UTC(),
	}
	attachStore.attachments[detached.ID] = detached

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	attachments, err := svc.List(context.Background(), listing.ID, ownerID)

	require.NoError(t, err)
	assert.Len(t, attachments, 1, "should return only the active (non-detached) attachment")
	assert.Equal(t, active.ID, attachments[0].ID)
}

// --- Detach tests ---

func TestAttachmentService_Detach_UploaderCanDetach(t *testing.T) {
	ownerID := uuid.New()
	uploaderID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	a := &domain.ListingAttachment{
		ID:         uuid.New(),
		ListingID:  listing.ID,
		UploaderID: uploaderID,
		CreatedAt:  time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	err := svc.Detach(context.Background(), listing.ID, a.ID, uploaderID)

	require.NoError(t, err)
	assert.NotNil(t, attachStore.attachments[a.ID].DetachedAt)
}

func TestAttachmentService_Detach_OwnerCanDetachAnyAttachment(t *testing.T) {
	ownerID := uuid.New()
	uploaderID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	a := &domain.ListingAttachment{
		ID:         uuid.New(),
		ListingID:  listing.ID,
		UploaderID: uploaderID, // different from owner
		CreatedAt:  time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	err := svc.Detach(context.Background(), listing.ID, a.ID, ownerID)

	require.NoError(t, err)
}

func TestAttachmentService_Detach_StrangerForbidden(t *testing.T) {
	ownerID := uuid.New()
	uploaderID := uuid.New()
	strangerID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	a := &domain.ListingAttachment{
		ID:         uuid.New(),
		ListingID:  listing.ID,
		UploaderID: uploaderID,
		CreatedAt:  time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	err := svc.Detach(context.Background(), listing.ID, a.ID, strangerID)

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrAttachmentForbidden))
}

func TestAttachmentService_Detach_WrongListingIDReturnsNotFound(t *testing.T) {
	ownerID := uuid.New()
	listing1 := makeOpenListing(ownerID)
	listing2 := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	// Attachment belongs to listing1.
	a := &domain.ListingAttachment{
		ID:         uuid.New(),
		ListingID:  listing1.ID,
		UploaderID: ownerID,
		CreatedAt:  time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing1, listing2),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	// Try to detach from listing2 (wrong listing) — should be ErrAttachmentNotFound.
	err := svc.Detach(context.Background(), listing2.ID, a.ID, ownerID)

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrAttachmentNotFound))
}

// TestAttachmentService_Attach_ClosedListingForbidsAttach verifies the status gate
// added in the review fix: Attach must return ErrListingNotOpen when the listing
// status is not OPEN, even if the caller is the owner or an approved collaborator.
func TestAttachmentService_Attach_ClosedListingForbidsAttach(t *testing.T) {
	tests := []struct {
		name   string
		status domain.ListingStatus
	}{
		{name: "closed listing", status: domain.ListingStatusClosed},
		{name: "awarded listing", status: domain.ListingStatusAwarded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ownerID := uuid.New()
			listing := &domain.Listing{
				ID:          uuid.New(),
				OwnerUserID: ownerID,
				Status:      tc.status,
				CreatedAt:   time.Now().UTC(),
				UpdatedAt:   time.Now().UTC(),
			}

			svc := newAttachmentService(
				newFakeAttachmentStore(),
				newFakeListingStore(listing),
				newFakeCollaboratorStore(),
				&fakeFileClient{},
			)

			_, err := svc.Attach(context.Background(), listing.ID, ownerID, validAttachInput())

			require.Error(t, err)
			assert.True(t, errors.Is(err, domain.ErrListingNotOpen),
				"expected ErrListingNotOpen for %s listing, got: %v", tc.name, err)
		})
	}
}

// --- DownloadURL tests ---

func TestAttachmentService_DownloadURL_OpenListingAnyoneCanPresign(t *testing.T) {
	ownerID := uuid.New()
	strangerID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	a := &domain.ListingAttachment{
		ID:        uuid.New(),
		ListingID: listing.ID,
		FileID:    uuid.New(),
		CreatedAt: time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	fileClient := &fakeFileClient{presignURL: "https://example.com/presigned"}

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		fileClient,
	)

	url, err := svc.DownloadURL(context.Background(), listing.ID, a.ID, strangerID)

	require.NoError(t, err)
	assert.Equal(t, "https://example.com/presigned", url)
}

// TestAttachmentService_DownloadURL_ClosedListingForbidsStranger mirrors
// TestAttachmentService_List_ClosedListingForbidsStranger for the DownloadURL path.
// A stranger (non-owner, non-collaborator) must be rejected on a non-OPEN listing.
func TestAttachmentService_DownloadURL_ClosedListingForbidsStranger(t *testing.T) {
	ownerID := uuid.New()
	strangerID := uuid.New()
	listing := &domain.Listing{
		ID:          uuid.New(),
		OwnerUserID: ownerID,
		Status:      domain.ListingStatusClosed,
	}

	attachStore := newFakeAttachmentStore()
	a := &domain.ListingAttachment{
		ID:        uuid.New(),
		ListingID: listing.ID,
		FileID:    uuid.New(),
		CreatedAt: time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		&fakeFileClient{},
	)

	_, err := svc.DownloadURL(context.Background(), listing.ID, a.ID, strangerID)

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrAttachmentForbidden),
		"stranger must be forbidden on a closed listing, got: %v", err)
}

func TestAttachmentService_DownloadURL_FileClientError(t *testing.T) {
	ownerID := uuid.New()
	listing := makeOpenListing(ownerID)

	attachStore := newFakeAttachmentStore()
	a := &domain.ListingAttachment{
		ID:        uuid.New(),
		ListingID: listing.ID,
		FileID:    uuid.New(),
		CreatedAt: time.Now().UTC(),
	}
	attachStore.attachments[a.ID] = a

	fileClient := &fakeFileClient{presignErr: domain.ErrUpstreamFile}

	svc := newAttachmentService(
		attachStore,
		newFakeListingStore(listing),
		newFakeCollaboratorStore(),
		fileClient,
	)

	_, err := svc.DownloadURL(context.Background(), listing.ID, a.ID, ownerID)

	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUpstreamFile))
}

// TestAttachmentService_Attach_NormalizesContentType asserts that Attach stores the
// canonical lower-case MIME type (without parameters) into the DB column, not the raw
// client-supplied value. This verifies the normalizedContentType-persist fix: if
// in.ContentType were stored instead of normalizedContentType, the assertion below
// would fail for "APPLICATION/PDF; charset=utf-8".
func TestAttachmentService_Attach_NormalizesContentType(t *testing.T) {
	tests := []struct {
		name            string
		rawContentType  string
		wantContentType string
	}{
		{
			name:            "uppercase MIME with charset parameter",
			rawContentType:  "APPLICATION/PDF; charset=utf-8",
			wantContentType: "application/pdf",
		},
		{
			name:            "mixed-case MIME with whitespace",
			rawContentType:  "  Application/PDF  ",
			wantContentType: "application/pdf",
		},
		{
			name:            "already canonical",
			rawContentType:  contentTypePDF,
			wantContentType: contentTypePDF,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ownerID := uuid.New()
			listing := makeOpenListing(ownerID)

			attachStore := newFakeAttachmentStore()

			svc := newAttachmentService(
				attachStore,
				newFakeListingStore(listing),
				newFakeCollaboratorStore(),
				&fakeFileClient{},
			)

			in := validAttachInput()
			in.ContentType = tc.rawContentType

			a, err := svc.Attach(context.Background(), listing.ID, ownerID, in)

			require.NoError(t, err)
			assert.Equal(t, tc.wantContentType, a.ContentType,
				"persisted ContentType must be normalized; raw value was %q", tc.rawContentType)

			// Also verify the stored attachment has the normalized value.
			stored := attachStore.attachments[a.ID]
			require.NotNil(t, stored)
			assert.Equal(t, tc.wantContentType, stored.ContentType,
				"stored attachment ContentType must be normalized")
		})
	}
}
