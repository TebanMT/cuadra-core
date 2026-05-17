package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/app"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ─── fakes (no DB) ─────────────────────────────────────────────────────────

type fpFakeTx struct{}

func (fpFakeTx) Execute(fn func(sharedDomain.Transaction) error) error {
	return fn(fpFakeTx{})
}

type fpFakeUoW struct{}

func (fpFakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return fpFakeTx{}, nil
}
func (fpFakeUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (fpFakeUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (fpFakeUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return fpFakeTx{}, nil
}
func (fpFakeUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(fpFakeTx{})
}

type fpFakeAudit struct{ entries []audit.Entry }

func (a *fpFakeAudit) Record(_ context.Context, _ sharedDomain.Transaction, e audit.Entry) error {
	a.entries = append(a.entries, e)
	return nil
}

type fpFakeMemberRepo struct {
	members map[uuid.UUID]*memberDomain.Member
}

func (r *fpFakeMemberRepo) Create(_ sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error) {
	r.members[m.ID] = m
	return m, nil
}
func (r *fpFakeMemberRepo) Update(_ sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error) {
	r.members[m.ID] = m
	return m, nil
}
func (r *fpFakeMemberRepo) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*memberDomain.Member, error) {
	m, ok := r.members[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return m, nil
}
func (r *fpFakeMemberRepo) ExistsByGymAndPhone(sharedDomain.Transaction, uuid.UUID, string) (bool, error) {
	return false, nil
}
func (r *fpFakeMemberRepo) PinHashCollidesInGym(sharedDomain.Transaction, uuid.UUID, string, *uuid.UUID) (bool, error) {
	return false, nil
}
func (r *fpFakeMemberRepo) NextFolio(sharedDomain.Transaction, uuid.UUID) (string, error) {
	return "test_001", nil
}
func (r *fpFakeMemberRepo) List(sharedDomain.Transaction, memRepo.ListQuery) ([]*memRepo.MemberWithMembership, int, error) {
	return nil, 0, nil
}
func (r *fpFakeMemberRepo) GetWithCurrentMembership(sharedDomain.Transaction, uuid.UUID, uuid.UUID) (*memRepo.MemberWithMembership, error) {
	return nil, nil
}
func (r *fpFakeMemberRepo) GetNamesByIDs(_ sharedDomain.Transaction, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string)
	for _, id := range ids {
		if m, ok := r.members[id]; ok {
			out[id] = m.FullName
		}
	}
	return out, nil
}

type fpFakeFpRepo struct {
	byMember map[uuid.UUID]*fpDomain.MemberFingerprint
	byGym    map[uuid.UUID][]*fpDomain.MemberFingerprint
	created  []*fpDomain.MemberFingerprint
}

func newFpFakeRepo() *fpFakeFpRepo {
	return &fpFakeFpRepo{
		byMember: map[uuid.UUID]*fpDomain.MemberFingerprint{},
		byGym:    map[uuid.UUID][]*fpDomain.MemberFingerprint{},
	}
}
func (r *fpFakeFpRepo) Create(_ sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	r.created = append(r.created, fp)
	r.byMember[fp.MemberID] = fp
	r.byGym[fp.GymID] = append(r.byGym[fp.GymID], fp)
	return fp, nil
}
func (r *fpFakeFpRepo) Update(_ sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	r.byMember[fp.MemberID] = fp
	return fp, nil
}
func (r *fpFakeFpRepo) GetByMember(_ sharedDomain.Transaction, memberID uuid.UUID) (*fpDomain.MemberFingerprint, error) {
	return r.byMember[memberID], nil
}
func (r *fpFakeFpRepo) ListByGym(_ sharedDomain.Transaction, gymID uuid.UUID) ([]*fpDomain.MemberFingerprint, error) {
	return r.byGym[gymID], nil
}

// stubReader is a minimal biometric.Reader for unit tests. It returns a
// pre-staged result from Identify; everything else is unused.
type stubReader struct {
	match *biometric.MatchResult
	err   error
	calls int
}

func (s *stubReader) Info() biometric.ReaderInfo { return biometric.ReaderInfo{} }
func (s *stubReader) OnConnect(func())           {}
func (s *stubReader) OnDisconnect(func())        {}
func (s *stubReader) Capture(context.Context) (*biometric.CaptureResult, error) {
	return nil, biometric.ErrNotAvailable
}
func (s *stubReader) Enroll(context.Context, int) (*biometric.CaptureResult, error) {
	return nil, biometric.ErrNotAvailable
}
func (s *stubReader) ExtractTemplate(context.Context, []byte) (*biometric.CaptureResult, error) {
	return nil, biometric.ErrNotAvailable
}
func (s *stubReader) Identify(_ context.Context, _ *biometric.CaptureResult, _ []biometric.EncryptedTemplate, _ float64) (*biometric.MatchResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.match, nil
}
func (s *stubReader) Available(context.Context) bool { return true }

// ─── helpers ───────────────────────────────────────────────────────────────

type fixture struct {
	uc        *app.RegisterFingerprint
	members   *fpFakeMemberRepo
	fps       *fpFakeFpRepo
	reader    *stubReader
	gym       uuid.UUID
	actor     uuid.UUID
	memberID  uuid.UUID
	otherID   uuid.UUID
	otherName string
}

func newFixture(t *testing.T, reader biometric.Reader) *fixture {
	t.Helper()
	gymID := uuid.New()
	actor := uuid.New()
	memberID := uuid.New()
	otherID := uuid.New()
	now := time.Now().UTC()

	member, err := memberDomain.NewMember(memberID, gymID, "m_001", "Pedro Soto", "5550000001", actor, now)
	if err != nil {
		t.Fatalf("new member: %v", err)
	}
	other, err := memberDomain.NewMember(otherID, gymID, "m_002", "Juan Pérez", "5550000002", actor, now)
	if err != nil {
		t.Fatalf("new other: %v", err)
	}

	members := &fpFakeMemberRepo{members: map[uuid.UUID]*memberDomain.Member{
		memberID: member,
		otherID:  other,
	}}
	fps := newFpFakeRepo()

	gmk := bcrypto.NewInMemoryGMKProvider()
	gmk.SetDeterministic(gymID, "unit-test-seed")

	uc := app.NewRegisterFingerprint(members, fps, gmk, fpFakeUoW{}, &fpFakeAudit{})
	var sr *stubReader
	if r, ok := reader.(*stubReader); ok {
		sr = r
	}
	if reader != nil {
		uc.WithReader(reader)
	}
	return &fixture{
		uc: uc, members: members, fps: fps, reader: sr,
		gym: gymID, actor: actor, memberID: memberID,
		otherID: otherID, otherName: other.FullName,
	}
}

func validInput(f *fixture, plain []byte) app.RegisterFingerprintInput {
	return app.RegisterFingerprintInput{
		GymID:           f.gym,
		ActorUserID:     f.actor,
		MemberID:        f.memberID,
		ConsentAccepted: true,
		Capture: &biometric.CaptureResult{
			Bytes:        append([]byte{}, plain...),
			Format:       fpDomain.FormatDP,
			QualityScore: 90,
		},
	}
}

// ─── tests ─────────────────────────────────────────────────────────────────

// Reader returns a match → use case must abort with ErrFingerprintCollision
// and surface the existing member's id+name in the CustomError data so the
// controller can build the modal payload.
func TestRegisterFingerprint_Collision_BlocksAndCarriesPayload(t *testing.T) {
	reader := &stubReader{match: &biometric.MatchResult{Score: 0.95}}
	f := newFixture(t, reader)
	// Reader matches the OTHER member.
	reader.match.MemberID = f.otherID.String()
	// Seed an enrolled template for the other member so the candidate set
	// is non-empty (Identify is only invoked when there are candidates).
	f.fps.byGym[f.gym] = []*fpDomain.MemberFingerprint{
		{ID: uuid.New(), GymID: f.gym, MemberID: f.otherID,
			TemplateEncrypted: []byte("juan-template-enc"), TemplateFormat: fpDomain.FormatDP},
	}

	_, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro-dedo-de-juan")))
	if err == nil {
		t.Fatalf("expected collision error, got nil")
	}
	if !errors.Is(err, fpDomain.ErrFingerprintCollision) {
		t.Fatalf("expected ErrFingerprintCollision in chain, got %v", err)
	}
	var ce sharedDomain.CustomError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CustomError, got %T", err)
	}
	if got := ce.Data["existing_member_id"]; got != f.otherID.String() {
		t.Errorf("existing_member_id = %v, want %v", got, f.otherID)
	}
	if got := ce.Data["existing_member_name"]; got != f.otherName {
		t.Errorf("existing_member_name = %v, want %q", got, f.otherName)
	}
	if len(f.fps.created) != 0 {
		t.Errorf("collision must NOT persist the fingerprint, got %d rows", len(f.fps.created))
	}
}

// Reader returns ErrNoMatch → use case proceeds normally and persists.
func TestRegisterFingerprint_NoMatch_PersistsNormally(t *testing.T) {
	reader := &stubReader{err: biometric.ErrNoMatch}
	f := newFixture(t, reader)
	// Pre-seed an "other" template so the candidate set is non-empty (forces
	// the Reader.Identify call).
	encrypted := []byte("ciphertext-of-juan")
	f.fps.byGym[f.gym] = []*fpDomain.MemberFingerprint{
		{ID: uuid.New(), GymID: f.gym, MemberID: f.otherID,
			TemplateEncrypted: encrypted, TemplateFormat: fpDomain.FormatDP},
	}

	out, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro-dedo-propio")))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil || out.MemberID != f.memberID {
		t.Errorf("unexpected output: %+v", out)
	}
	if reader.calls != 1 {
		t.Errorf("expected exactly 1 Identify call, got %d", reader.calls)
	}
	if len(f.fps.created) != 1 {
		t.Errorf("expected fingerprint persisted, got %d rows", len(f.fps.created))
	}
}

// Re-enrollment of a member that already has a fingerprint: the candidate
// set must exclude the target member's row so Identify isn't invoked with a
// self-match. Today the re-enroll itself is blocked by ErrFingerprintAlreadySet
// — this test guards the exclusion logic so a future re-enroll flow won't
// regress into self-collision.
func TestRegisterFingerprint_Collision_ExcludesSelfFromCandidates(t *testing.T) {
	reader := &stubReader{err: biometric.ErrNoMatch}
	f := newFixture(t, reader)
	// Member's own template lives in the gym set — must be filtered out.
	f.fps.byGym[f.gym] = []*fpDomain.MemberFingerprint{
		{ID: uuid.New(), GymID: f.gym, MemberID: f.memberID,
			TemplateEncrypted: []byte("self-template"), TemplateFormat: fpDomain.FormatDP},
	}
	// Self is already enrolled → today this returns ErrFingerprintAlreadySet
	// AFTER the collision check skipped (empty candidate set means Identify
	// is never called).
	f.fps.byMember[f.memberID] = f.fps.byGym[f.gym][0]

	_, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro-mismo")))
	if err == nil {
		t.Fatalf("expected ErrFingerprintAlreadySet, got nil")
	}
	if !errors.Is(err, fpDomain.ErrFingerprintAlreadySet) {
		t.Fatalf("expected ErrFingerprintAlreadySet, got %v", err)
	}
	if reader.calls != 0 {
		t.Errorf("Reader.Identify must NOT be called when candidate set is empty after self-exclusion, got %d calls", reader.calls)
	}
}

// Reader nil → use case skips collision check entirely. Validates the cloud
// build path (no SDK wired, enrollment still functional for dashboard/tests).
func TestRegisterFingerprint_ReaderNil_SkipsCollisionCheck(t *testing.T) {
	f := newFixture(t, nil) // no reader
	// Even with another member's template present, no Identify runs.
	f.fps.byGym[f.gym] = []*fpDomain.MemberFingerprint{
		{ID: uuid.New(), GymID: f.gym, MemberID: f.otherID,
			TemplateEncrypted: []byte("juan-template"), TemplateFormat: fpDomain.FormatDP},
	}

	out, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro")))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil {
		t.Fatalf("expected output, got nil")
	}
}

// Reader returns ErrNotAvailable (SDK absent at runtime) → check skipped,
// enrollment proceeds. Same behavior as Reader nil, just at a different layer.
func TestRegisterFingerprint_ReaderNotAvailable_SkipsCollisionCheck(t *testing.T) {
	reader := &stubReader{err: biometric.ErrNotAvailable}
	f := newFixture(t, reader)
	f.fps.byGym[f.gym] = []*fpDomain.MemberFingerprint{
		{ID: uuid.New(), GymID: f.gym, MemberID: f.otherID,
			TemplateEncrypted: []byte("juan-template"), TemplateFormat: fpDomain.FormatDP},
	}

	out, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro")))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil {
		t.Fatalf("expected output, got nil")
	}
	if reader.calls != 1 {
		t.Errorf("expected 1 Identify call (returning ErrNotAvailable), got %d", reader.calls)
	}
	if len(f.fps.created) != 1 {
		t.Errorf("expected fingerprint persisted despite reader-unavailable, got %d rows", len(f.fps.created))
	}
}
