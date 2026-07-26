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
func (r *fpFakeMemberRepo) MemberNumberExistsInGym(sharedDomain.Transaction, uuid.UUID, int, *uuid.UUID) (bool, error) {
	return false, nil
}
func (r *fpFakeMemberRepo) ListUsedMemberNumbers(sharedDomain.Transaction, uuid.UUID) ([]int, error) {
	return nil, nil
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

func (r *fpFakeMemberRepo) GetContactsByIDs(_ sharedDomain.Transaction, gymID uuid.UUID, ids []uuid.UUID) ([]memRepo.MemberContact, error) {
	out := make([]memRepo.MemberContact, 0, len(ids))
	for _, id := range ids {
		if m, ok := r.members[id]; ok && m.GymID == gymID {
			out = append(out, memRepo.MemberContact{ID: m.ID, FullName: m.FullName, Phone: m.Phone})
		}
	}
	return out, nil
}

type fpFakeFpRepo struct {
	byMember map[uuid.UUID][]*fpDomain.MemberFingerprint
	byGym    map[uuid.UUID][]*fpDomain.MemberFingerprint
	created  []*fpDomain.MemberFingerprint
}

func newFpFakeRepo() *fpFakeFpRepo {
	return &fpFakeFpRepo{
		byMember: map[uuid.UUID][]*fpDomain.MemberFingerprint{},
		byGym:    map[uuid.UUID][]*fpDomain.MemberFingerprint{},
	}
}
func (r *fpFakeFpRepo) Create(_ sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	r.created = append(r.created, fp)
	r.byMember[fp.MemberID] = append(r.byMember[fp.MemberID], fp)
	r.byGym[fp.GymID] = append(r.byGym[fp.GymID], fp)
	return fp, nil
}
func (r *fpFakeFpRepo) Update(_ sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	return fp, nil
}
func (r *fpFakeFpRepo) ListByMember(_ sharedDomain.Transaction, memberID uuid.UUID) ([]*fpDomain.MemberFingerprint, error) {
	return r.byMember[memberID], nil
}
func (r *fpFakeFpRepo) ListByGym(_ sharedDomain.Transaction, gymID uuid.UUID) ([]*fpDomain.MemberFingerprint, error) {
	return r.byGym[gymID], nil
}

// stubMatcher is a minimal app.FingerprintMatcher for unit tests: returns a
// pre-staged member id (uuid.Nil = no match) or error from IdentifyFMD.
type stubMatcher struct {
	match uuid.UUID
	err   error
	calls int
}

func (s *stubMatcher) IdentifyFMD(_ context.Context, _ string) (uuid.UUID, error) {
	s.calls++
	if s.err != nil {
		return uuid.Nil, s.err
	}
	return s.match, nil
}

// ─── helpers ───────────────────────────────────────────────────────────────

type fixture struct {
	uc        *app.RegisterFingerprint
	members   *fpFakeMemberRepo
	fps       *fpFakeFpRepo
	matcher   *stubMatcher
	gym       uuid.UUID
	actor     uuid.UUID
	memberID  uuid.UUID
	otherID   uuid.UUID
	otherName string
}

func newFixture(t *testing.T, matcher *stubMatcher) *fixture {
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
	if matcher != nil {
		uc.WithMatcher(matcher)
	}
	return &fixture{
		uc: uc, members: members, fps: fps, matcher: matcher,
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
		Captures: []*biometric.CaptureResult{{
			Bytes:        append([]byte{}, plain...),
			Format:       fpDomain.FormatDP,
			QualityScore: 90,
		}},
	}
}

// ─── tests ─────────────────────────────────────────────────────────────────

// Matcher returns another member → use case must abort with
// ErrFingerprintCollision and surface the existing member's id+name in the
// CustomError data so the controller/hub can build the modal payload.
func TestRegisterFingerprint_Collision_BlocksAndCarriesPayload(t *testing.T) {
	f := newFixture(t, &stubMatcher{})
	// Matcher matches the OTHER member.
	f.matcher.match = f.otherID

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

// Matcher returns no match (uuid.Nil) → use case proceeds normally and
// persists.
func TestRegisterFingerprint_NoMatch_PersistsNormally(t *testing.T) {
	f := newFixture(t, &stubMatcher{})

	out, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro-dedo-propio")))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil || out.MemberID != f.memberID {
		t.Errorf("unexpected output: %+v", out)
	}
	if f.matcher.calls != 1 {
		t.Errorf("expected exactly 1 IdentifyFMD call, got %d", f.matcher.calls)
	}
	if len(f.fps.created) != 1 {
		t.Errorf("expected fingerprint persisted, got %d rows", len(f.fps.created))
	}
}

// A self-match (the matcher resolves the probe to the TARGET member) is not
// a collision — guards the future re-enroll flow against self-collision now
// that the helper's gallery can't exclude the target per-call.
func TestRegisterFingerprint_Collision_SelfMatchIsNotCollision(t *testing.T) {
	f := newFixture(t, &stubMatcher{})
	f.matcher.match = f.memberID

	out, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro-mismo")))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil || len(f.fps.created) != 1 {
		t.Errorf("self-match must persist normally, got out=%+v rows=%d", out, len(f.fps.created))
	}
}

// Matcher nil → use case skips collision check entirely. Validates the cloud
// build path (no engine wired, enrollment still functional for dashboard/tests).
func TestRegisterFingerprint_MatcherNil_SkipsCollisionCheck(t *testing.T) {
	f := newFixture(t, nil) // no matcher

	out, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro")))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil {
		t.Fatalf("expected output, got nil")
	}
}

// Matcher returns ErrNotAvailable (engine down at runtime) → check skipped,
// enrollment proceeds. Same behavior as Matcher nil, just at a different layer.
func TestRegisterFingerprint_MatcherNotAvailable_SkipsCollisionCheck(t *testing.T) {
	f := newFixture(t, &stubMatcher{err: biometric.ErrNotAvailable})

	out, err := f.uc.Execute(context.Background(), validInput(f, []byte("template-pedro")))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil {
		t.Fatalf("expected output, got nil")
	}
	if f.matcher.calls != 1 {
		t.Errorf("expected 1 IdentifyFMD call (returning ErrNotAvailable), got %d", f.matcher.calls)
	}
	if len(f.fps.created) != 1 {
		t.Errorf("expected fingerprint persisted despite engine-unavailable, got %d rows", len(f.fps.created))
	}
}

// Enrolling with 3 captures of the same finger persists all 3 as separate
// templates in one atomic call — the data model behind UC-028 best-of-3
// matching. Output carries every id and the best quality across captures.
// (El flujo de sesión tinta-bio manda 1 solo FMD de enrollment, pero el use
// case sigue aceptando 1..MaxFingerprintsPerMember.)
func TestRegisterFingerprint_ThreeCaptures_PersistsAll(t *testing.T) {
	f := newFixture(t, &stubMatcher{})

	in := validInput(f, []byte("template-pedro"))
	in.Captures = []*biometric.CaptureResult{
		{Bytes: []byte("cap-1"), Format: fpDomain.FormatDP, QualityScore: 85},
		{Bytes: []byte("cap-2"), Format: fpDomain.FormatDP, QualityScore: 91},
		{Bytes: []byte("cap-3"), Format: fpDomain.FormatDP, QualityScore: 78},
	}

	out, err := f.uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(f.fps.created) != 3 {
		t.Errorf("expected 3 templates persisted, got %d", len(f.fps.created))
	}
	if len(out.FingerprintIDs) != 3 {
		t.Errorf("expected 3 fingerprint ids, got %d", len(out.FingerprintIDs))
	}
	if out.QualityScore == nil || *out.QualityScore != 91 {
		t.Errorf("expected best quality 91, got %v", out.QualityScore)
	}
}

// More than MaxFingerprintsPerMember captures is a validation error — the FE
// always sends exactly 3, so this guards a misbehaving client.
func TestRegisterFingerprint_TooManyCaptures_Rejected(t *testing.T) {
	f := newFixture(t, nil)

	in := validInput(f, []byte("x"))
	in.Captures = make([]*biometric.CaptureResult, fpDomain.MaxFingerprintsPerMember+1)
	for i := range in.Captures {
		in.Captures[i] = &biometric.CaptureResult{Bytes: []byte("c"), QualityScore: 80}
	}

	if _, err := f.uc.Execute(context.Background(), in); !errors.Is(err, fpDomain.ErrTooManyCaptures) {
		t.Fatalf("expected ErrTooManyCaptures, got %v", err)
	}
	if len(f.fps.created) != 0 {
		t.Errorf("rejected enroll must persist nothing, got %d rows", len(f.fps.created))
	}
}
