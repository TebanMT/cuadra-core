package fingerprint_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
)

func TestNewMemberFingerprint_HappyPath(t *testing.T) {
	id, gym, member, registeredBy := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	score := 87
	now := time.Now().UTC()

	fp, err := fingerprint.NewMemberFingerprint(id, gym, member, registeredBy, []byte("encrypted-blob"), "", &score, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if fp.TemplateFormat != fingerprint.FormatDP {
		t.Errorf("default format should be %s, got %s", fingerprint.FormatDP, fp.TemplateFormat)
	}
	if fp.Version != 1 {
		t.Errorf("initial version must be 1, got %d", fp.Version)
	}
	if fp.QualityScore == nil || *fp.QualityScore != 87 {
		t.Errorf("quality not persisted: %+v", fp.QualityScore)
	}
	if !fp.CreatedAt.Equal(now) {
		t.Errorf("created_at not seeded")
	}
}

func TestNewMemberFingerprint_RejectsBadInput(t *testing.T) {
	good := []byte("blob")
	now := time.Now()
	cases := []struct {
		name                string
		id, gym, member, by uuid.UUID
		blob                []byte
		want                error
	}{
		{"nil id", uuid.Nil, uuid.New(), uuid.New(), uuid.New(), good, fingerprint.ErrInvalidIdentifiers},
		{"nil gym", uuid.New(), uuid.Nil, uuid.New(), uuid.New(), good, fingerprint.ErrInvalidIdentifiers},
		{"nil member", uuid.New(), uuid.New(), uuid.Nil, uuid.New(), good, fingerprint.ErrInvalidIdentifiers},
		{"nil registered_by", uuid.New(), uuid.New(), uuid.New(), uuid.Nil, good, fingerprint.ErrInvalidIdentifiers},
		{"empty blob", uuid.New(), uuid.New(), uuid.New(), uuid.New(), nil, fingerprint.ErrEmptyTemplate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fingerprint.NewMemberFingerprint(tc.id, tc.gym, tc.member, tc.by, tc.blob, "", nil, now)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewMemberFingerprint_RejectsLowQuality(t *testing.T) {
	low := fingerprint.QualityScoreFloor - 1
	_, err := fingerprint.NewMemberFingerprint(
		uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		[]byte("blob"), "", &low, time.Now(),
	)
	if !errors.Is(err, fingerprint.ErrQualityBelowFloor) {
		t.Errorf("expected ErrQualityBelowFloor, got %v", err)
	}
}

func TestSoftDelete_BumpsVersion(t *testing.T) {
	fp, _ := fingerprint.NewMemberFingerprint(
		uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		[]byte("blob"), "", nil, time.Now(),
	)
	v0 := fp.Version
	fp.SoftDelete(time.Now())
	if fp.DeletedAt == nil {
		t.Errorf("DeletedAt should be set")
	}
	if fp.Version != v0+1 {
		t.Errorf("Version should bump from %d to %d, got %d", v0, v0+1, fp.Version)
	}
}
