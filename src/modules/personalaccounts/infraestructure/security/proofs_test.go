package security

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalSecurity "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/google/uuid"
)

const requestDigest = "2668abddc6b1b2968c52c3f83129e44e3bd1ca7b6c92bd04d5c4da070b46138a"

func sampleDeviceProof() personalSecurity.DeviceProof {
	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	digest, _ := hex.DecodeString(requestDigest)
	return personalSecurity.DeviceProof{Purpose: personalSecurity.RevokeDevices, AccountID: accountID, SessionID: sessionID, DeviceID: deviceID, ProofID: proofID, Nonce: nonce, RequestHash: [32]byte(digest), ExpiresAt: time.Date(2026, 10, 19, 1, 0, 0, 100*int(time.Millisecond), time.UTC)}
}

// Golden values were independently computed with S00's Python json.dumps:
// sorted keys, compact separators, ensure_ascii=False. Unicode and nested
// member order deliberately exercise differences from Go's default JSON.
func TestS00CanonicalGoldenVectors(t *testing.T) {
	body := []byte("{\"z\":[0,true,null],\"a\":\"á<&>\\u2028\\u2029\\n\"}")
	digest, err := RequestHash("POST", "/me/devices/"+deviceID.String()+"/revoke", operationID, body)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(digest[:]) != requestDigest {
		t.Fatalf("S00 hash mismatch: %x", digest)
	}
	canonicalBody := []byte("{ \"a\":\"á<&>\\u2028\\u2029\\n\", \"z\" : [0, true, null] }")
	reordered, err := RequestHash("POST", "/me/devices/"+deviceID.String()+"/revoke", operationID, canonicalBody)
	if err != nil || reordered != digest {
		t.Fatal("member order changed hash", err)
	}
	payload, err := DeviceProofPayload(sampleDeviceProof())
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"account_id":"11111111-1111-4111-8111-111111111111","device_id":"33333333-3333-4333-8333-333333333333","expires_at":"2026-10-19T01:00:00.100Z","nonce":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","proof_id":"44444444-4444-4444-8444-444444444444","purpose":"revoke_devices","request_hash":"2668abddc6b1b2968c52c3f83129e44e3bd1ca7b6c92bd04d5c4da070b46138a","session_id":"22222222-2222-4222-8222-222222222222","v":1}`
	if string(payload) != expected {
		t.Fatalf("canonical proof mismatch: %s", payload)
	}
	refresh := personalSecurity.RefreshProof{OperationID: operationID, SessionID: sessionID, Generation: 9007199254740993, RefreshHash: digest}
	payload, err = RefreshProofPayload(refresh)
	if err != nil {
		t.Fatal(err)
	}
	expected = `{"generation":"9007199254740993","operation_id":"55555555-5555-4555-8555-555555555555","purpose":"refresh","refresh_hash":"2668abddc6b1b2968c52c3f83129e44e3bd1ca7b6c92bd04d5c4da070b46138a","session_id":"22222222-2222-4222-8222-222222222222","v":1}`
	if string(payload) != expected {
		t.Fatalf("canonical refresh mismatch: %s", payload)
	}
}

func TestRequestHashBindsConcreteRequestAndRejectsAmbiguity(t *testing.T) {
	base, err := RequestHash("POST", "/me/devices/revoke", operationID, []byte(`{"scope":"all"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		method, path string
		operation    uuid.UUID
		body         string
	}{
		{"DELETE", "/me/devices/revoke", operationID, `{"scope":"all"}`},
		{"POST", "/me/devices/" + deviceID.String() + "/revoke", operationID, `{"scope":"all"}`},
		{"POST", "/me/devices/revoke", proofID, `{"scope":"all"}`},
		{"POST", "/me/devices/revoke", operationID, `{"scope":"others"}`},
	} {
		digest, err := RequestHash(input.method, input.path, input.operation, []byte(input.body))
		if err != nil || digest == base {
			t.Fatal("unbound request field", err)
		}
	}
	for _, body := range []string{`{"a":1,"a":2}`, `{"nested":{"a":1,"a":1}}`, `{"a":1.0}`, `{"a":1e2}`, `{} {}`, `{"a":`, `{"a":"\ud800"}`, `{"a":"\udc00"}`, `{"a":"\ud800\u0061"}`, "{\"a\":\"\xff\"}"} {
		if _, err := RequestHash("POST", "/me/devices/revoke", operationID, []byte(body)); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatalf("ambiguous/invalid body accepted %q: %v", body, err)
		}
	}
	utf8Hash, err := RequestHash("POST", "/me/devices/revoke", operationID, []byte(`{"a":"💪"}`))
	if err != nil {
		t.Fatal(err)
	}
	escapedHash, err := RequestHash("POST", "/me/devices/revoke", operationID, []byte(`{"a":"\ud83d\udcaa"}`))
	if err != nil || escapedHash != utf8Hash {
		t.Fatal("valid surrogate pair was not canonicalized", err)
	}
}

func TestDeviceProofSignatureCannotCrossContext(t *testing.T) {
	proof := sampleDeviceProof()
	payload, err := DeviceProofPayload(proof)
	if err != nil {
		t.Fatal(err)
	}
	key := testSigningKey()
	public := key.Public().(ed25519.PublicKey)
	signature := rawURL.EncodeToString(ed25519.Sign(key, payload))
	service := DeviceProofs{}
	if err := service.VerifyDevice(public, signature, proof, testNow); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*personalSecurity.DeviceProof){
		"account": func(p *personalSecurity.DeviceProof) { p.AccountID = operationID },
		"session": func(p *personalSecurity.DeviceProof) { p.SessionID = operationID },
		"device":  func(p *personalSecurity.DeviceProof) { p.DeviceID = operationID },
		"proof":   func(p *personalSecurity.DeviceProof) { p.ProofID = operationID },
		"purpose": func(p *personalSecurity.DeviceProof) { p.Purpose = personalSecurity.VaultUnlock },
		"nonce":   func(p *personalSecurity.DeviceProof) { p.Nonce = bytes.Repeat([]byte{9}, 32) },
		"request": func(p *personalSecurity.DeviceProof) { p.RequestHash[0] ^= 1 },
		"expiry":  func(p *personalSecurity.DeviceProof) { p.ExpiresAt = p.ExpiresAt.Add(time.Second) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := proof
			mutate(&changed)
			if err := service.VerifyDevice(public, signature, changed, testNow); !errors.Is(err, personalErrors.ErrInvalidProof) {
				t.Fatal("cross-context signature accepted", err)
			}
		})
	}
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)).Public().(ed25519.PublicKey)
	for _, pub := range [][]byte{nil, public[:31], other} {
		if err := service.VerifyDevice(pub, signature, proof, testNow); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatal("invalid signer accepted", err)
		}
	}
	for _, sig := range []string{"", signature + "=", signature[:85], "!" + signature[1:]} {
		if err := service.VerifyDevice(public, sig, proof, testNow); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatal("invalid signature encoding accepted", err)
		}
	}
	for _, now := range []time.Time{proof.ExpiresAt, proof.ExpiresAt.Add(time.Second), proof.ExpiresAt.Add(-personalSecurity.ProofTTL - time.Millisecond)} {
		if err := service.VerifyDevice(public, signature, proof, now); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatal("expired/long-lived proof accepted", err)
		}
	}
}

func TestResumeAndRefreshProofsBindTheirContext(t *testing.T) {
	key := testSigningKey()
	public := key.Public().(ed25519.PublicKey)
	service := DeviceProofs{}
	proof := sampleDeviceProof()
	resume := personalSecurity.ResumeProof{AttemptID: operationID, DeviceID: deviceID, Nonce: proof.Nonce, ExpiresAt: proof.ExpiresAt}
	payload, err := ResumeProofPayload(resume)
	if err != nil {
		t.Fatal(err)
	}
	signature := rawURL.EncodeToString(ed25519.Sign(key, payload))
	if err := service.VerifyResume(public, signature, resume, testNow); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*personalSecurity.ResumeProof){
		func(p *personalSecurity.ResumeProof) { p.AttemptID = proofID }, func(p *personalSecurity.ResumeProof) { p.DeviceID = proofID },
		func(p *personalSecurity.ResumeProof) { p.Nonce = bytes.Repeat([]byte{9}, 32) }, func(p *personalSecurity.ResumeProof) { p.ExpiresAt = p.ExpiresAt.Add(time.Second) },
	} {
		changed := resume
		mutate(&changed)
		if err := service.VerifyResume(public, signature, changed, testNow); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatal("resume context changed without invalidating signature", err)
		}
	}
	if err := service.VerifyResume(public, signature, resume, resume.ExpiresAt); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("expired resume proof accepted", err)
	}
	if err := service.VerifyDevice(public, signature, proof, testNow); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("resume signature accepted as recent device proof", err)
	}
	refresh := personalSecurity.RefreshProof{OperationID: operationID, SessionID: sessionID, Generation: 1, RefreshHash: proof.RequestHash}
	payload, err = RefreshProofPayload(refresh)
	if err != nil {
		t.Fatal(err)
	}
	signature = rawURL.EncodeToString(ed25519.Sign(key, payload))
	if err := service.VerifyRefresh(public, signature, refresh); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*personalSecurity.RefreshProof){
		func(p *personalSecurity.RefreshProof) { p.OperationID = proofID }, func(p *personalSecurity.RefreshProof) { p.SessionID = proofID },
		func(p *personalSecurity.RefreshProof) { p.Generation++ }, func(p *personalSecurity.RefreshProof) { p.RefreshHash[0] ^= 1 },
	} {
		changed := refresh
		mutate(&changed)
		if err := service.VerifyRefresh(public, signature, changed); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatal("refresh context changed without invalidating signature", err)
		}
	}
}

func TestProofTimestampContract(t *testing.T) {
	for _, tc := range []struct {
		at       time.Time
		expected string
	}{
		{testNow, "2026-10-19T00:56:00.000Z"},
		{testNow.Add(100 * time.Millisecond), "2026-10-19T00:56:00.100Z"},
		{testNow.In(time.FixedZone("test", -6*3600)), "2026-10-19T00:56:00.000Z"},
	} {
		got, err := proofTimestamp(tc.at)
		if err != nil || got != tc.expected {
			t.Fatalf("timestamp %q %v", got, err)
		}
	}
	if _, err := proofTimestamp(testNow.Add(time.Microsecond)); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("sub-ms expiry accepted", err)
	}
}

func TestWholeSecondProofsMatchPublicMillisecondsAndNormalizeEquivalentInput(t *testing.T) {
	// Independent wire vectors use the same public timestamp a client receives
	// from issuance; do not obtain the signed bytes from the payload helper.
	deviceWire := []byte(`{"account_id":"11111111-1111-4111-8111-111111111111","device_id":"33333333-3333-4333-8333-333333333333","expires_at":"2026-10-19T01:00:00.000Z","nonce":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","proof_id":"44444444-4444-4444-8444-444444444444","purpose":"revoke_devices","request_hash":"2668abddc6b1b2968c52c3f83129e44e3bd1ca7b6c92bd04d5c4da070b46138a","session_id":"22222222-2222-4222-8222-222222222222","v":1}`)
	resumeWire := []byte(`{"attempt_id":"55555555-5555-4555-8555-555555555555","device_id":"33333333-3333-4333-8333-333333333333","expires_at":"2026-10-19T01:00:00.000Z","nonce":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","purpose":"resume_device","v":1}`)
	key := testSigningKey()
	public := key.Public().(ed25519.PublicKey)
	deviceSignature := rawURL.EncodeToString(ed25519.Sign(key, deviceWire))
	resumeSignature := rawURL.EncodeToString(ed25519.Sign(key, resumeWire))
	for _, input := range []string{"2026-10-19T01:00:00Z", "2026-10-19T01:00:00.000Z"} {
		at, err := time.Parse(time.RFC3339Nano, input)
		if err != nil {
			t.Fatal(err)
		}
		proof := sampleDeviceProof()
		proof.ExpiresAt = at
		payload, err := DeviceProofPayload(proof)
		if err != nil || !bytes.Equal(payload, deviceWire) {
			t.Fatal("device timestamp disagrees with wire", err)
		}
		if err = (DeviceProofs{}).VerifyDevice(public, deviceSignature, proof, testNow); err != nil {
			t.Fatal(err)
		}
		resume := personalSecurity.ResumeProof{AttemptID: operationID, DeviceID: deviceID, Nonce: proof.Nonce, ExpiresAt: at}
		payload, err = ResumeProofPayload(resume)
		if err != nil || !bytes.Equal(payload, resumeWire) {
			t.Fatal("resume timestamp disagrees with wire", err)
		}
		if err = (DeviceProofs{}).VerifyResume(public, resumeSignature, resume, testNow); err != nil {
			t.Fatal(err)
		}
	}
}
