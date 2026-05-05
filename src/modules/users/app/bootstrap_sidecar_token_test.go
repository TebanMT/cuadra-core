package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
)

type fakeStore struct {
	active   *sidecartoken.Credential
	inserted *struct {
		gymID, clientID, userID uuid.UUID
		hash                    []byte
		deviceLabel             string
	}
	insertErr error
	findErr   error
}

func (f *fakeStore) LookupActiveByHash(context.Context, []byte) (sidecartoken.Credential, error) {
	return sidecartoken.Credential{}, sidecartoken.ErrNotFound
}
func (f *fakeStore) TouchLastSeen(context.Context, uuid.UUID) error { return nil }
func (f *fakeStore) FindActive(_ context.Context, gymID, clientID uuid.UUID) (sidecartoken.Credential, error) {
	if f.findErr != nil {
		return sidecartoken.Credential{}, f.findErr
	}
	if f.active != nil && f.active.GymID == gymID && f.active.ClientID == clientID {
		return *f.active, nil
	}
	return sidecartoken.Credential{}, sidecartoken.ErrNotFound
}
func (f *fakeStore) Insert(_ context.Context, gymID, clientID, userID uuid.UUID, hash []byte, deviceLabel string) (sidecartoken.Credential, error) {
	if f.insertErr != nil {
		return sidecartoken.Credential{}, f.insertErr
	}
	f.inserted = &struct {
		gymID, clientID, userID uuid.UUID
		hash                    []byte
		deviceLabel             string
	}{gymID, clientID, userID, hash, deviceLabel}
	return sidecartoken.Credential{ID: uuid.New(), GymID: gymID, ClientID: clientID, UserID: userID}, nil
}
func (f *fakeStore) RevokeActive(context.Context, uuid.UUID, uuid.UUID) error  { return nil }
func (f *fakeStore) RevokeIdle(context.Context, time.Time) (int64, error)      { return 0, nil }

func TestBootstrap_MintsWhenNoActiveCredential(t *testing.T) {
	store := &fakeStore{}
	uc := NewBootstrapSidecarToken(store, nil)
	tok, err := uc.Execute(context.Background(), BootstrapSidecarTokenInput{
		GymID: uuid.New(), UserID: uuid.New(), ClientID: uuid.New(), DeviceLabel: "laptop-1",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tok == "" || !sidecartoken.HasPrefix(tok) {
		t.Errorf("expected sk_live_* token, got %q", tok)
	}
	if store.inserted == nil {
		t.Errorf("expected Insert to be called")
	}
	if store.inserted.deviceLabel != "laptop-1" {
		t.Errorf("device_label not propagated: %q", store.inserted.deviceLabel)
	}
}

func TestBootstrap_NoOpWhenActiveExists(t *testing.T) {
	gymID := uuid.New()
	clientID := uuid.New()
	store := &fakeStore{active: &sidecartoken.Credential{
		GymID: gymID, ClientID: clientID, UserID: uuid.New(),
	}}
	uc := NewBootstrapSidecarToken(store, nil)
	tok, err := uc.Execute(context.Background(), BootstrapSidecarTokenInput{
		GymID: gymID, UserID: uuid.New(), ClientID: clientID,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tok != "" {
		t.Errorf("expected empty token (no re-emission), got %q", tok)
	}
	if store.inserted != nil {
		t.Errorf("Insert should NOT be called when active credential exists")
	}
}

func TestBootstrap_ReturnsErrorOnLookupFailure(t *testing.T) {
	store := &fakeStore{findErr: errors.New("db down")}
	uc := NewBootstrapSidecarToken(store, nil)
	if _, err := uc.Execute(context.Background(), BootstrapSidecarTokenInput{
		GymID: uuid.New(), UserID: uuid.New(), ClientID: uuid.New(),
	}); err == nil {
		t.Errorf("expected error from FindActive failure")
	}
}

func TestBootstrap_RejectsZeroIDs(t *testing.T) {
	store := &fakeStore{}
	uc := NewBootstrapSidecarToken(store, nil)
	if _, err := uc.Execute(context.Background(), BootstrapSidecarTokenInput{}); err == nil {
		t.Errorf("expected validation error for zero ids")
	}
}

func TestBootstrap_NoStoreReturnsEmpty(t *testing.T) {
	uc := NewBootstrapSidecarToken(nil, nil)
	tok, err := uc.Execute(context.Background(), BootstrapSidecarTokenInput{
		GymID: uuid.New(), UserID: uuid.New(), ClientID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("expected no error when Store=nil, got %v", err)
	}
	if tok != "" {
		t.Errorf("expected empty token when Store=nil, got %q", tok)
	}
}
