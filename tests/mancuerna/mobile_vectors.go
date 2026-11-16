//go:build ignore

// Synthetic public test vectors only. No network, database or environment reads.
// emit produces Go-signed fixtures; verify checks signatures emitted by JS.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	proofs "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/google/uuid"
)

const (
	account   = "11111111-1111-4111-8111-111111111111"
	session   = "22222222-2222-4222-8222-222222222222"
	device    = "33333333-3333-4333-8333-333333333333"
	proofID   = "44444444-4444-4444-8444-444444444444"
	operation = "55555555-5555-4555-8555-555555555555"
)

type requestVector struct {
	Name          string          `json:"name"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	OperationID   string          `json:"operation_id"`
	Body          json.RawMessage `json:"body"`
	CanonicalBody string          `json:"canonical_body"`
	Hash          string          `json:"hash"`
}
type proofInput struct {
	AccountID   string `json:"account_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	ProofID     string `json:"proof_id,omitempty"`
	AttemptID   string `json:"attempt_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
	Nonce       string `json:"nonce,omitempty"`
	RequestHash string `json:"request_hash,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Generation  string `json:"generation,omitempty"`
	RefreshHash string `json:"refresh_hash,omitempty"`
}
type proofVector struct {
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	Input     proofInput `json:"input"`
	Payload   string     `json:"payload"`
	Signature string     `json:"signature"`
}
type vectorDocument struct {
	Version          int             `json:"version"`
	SyntheticSeedHex string          `json:"synthetic_seed_hex"`
	PublicKey        string          `json:"public_key"`
	RefreshToken     string          `json:"refresh_token"`
	RefreshHash      string          `json:"refresh_hash"`
	Requests         []requestVector `json:"requests"`
	Proofs           []proofVector   `json:"proofs"`
}

func fixture() vectorDocument {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	key := ed25519.NewKeyFromSeed(seed)
	nonce := base64.RawURLEncoding.EncodeToString(seed)
	token := "synthetic_refresh_" + nonce
	digest := sha256.Sum256([]byte(token))
	doc := vectorDocument{Version: 1, SyntheticSeedHex: hex.EncodeToString(seed), PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), RefreshToken: token, RefreshHash: hex.EncodeToString(digest[:])}
	doc.Requests = []requestVector{
		{Name: "unicode", Method: "POST", Path: "/me/devices/" + device + "/revoke", OperationID: operation, Body: json.RawMessage(`{"z":[0,true,null],"a":"á<&>\u2028\u2029\n"}`)},
		{Name: "empty-concrete-body", Method: "POST", Path: "/me/devices/" + device + "/revoke", OperationID: operation, Body: json.RawMessage(`{}`)},
		{Name: "other-devices", Method: "POST", Path: "/me/devices/revoke", OperationID: operation, Body: json.RawMessage(`{"scope":"others"}`)},
		{Name: "unicode-scalar-order", Method: "POST", Path: "/commands", OperationID: operation, Body: json.RawMessage(`{"𐀀":{"z":"💪","é":"é"},"\uffff":-0,"a":["0.000","9007199254740993","\u0001\b\t\n\f\r\"\\"]}`)},
	}
	for i := range doc.Requests {
		r := &doc.Requests[i]
		hash, err := crypto.RequestHash(r.Method, r.Path, uuid.MustParse(r.OperationID), r.Body)
		must(err)
		r.Hash = hex.EncodeToString(hash[:])
		body, err := (crypto.JSONCanonicalizer{}).Canonicalize(r.Body)
		must(err)
		r.CanonicalBody = string(body)
	}
	if doc.Requests[0].Hash != "2668abddc6b1b2968c52c3f83129e44e3bd1ca7b6c92bd04d5c4da070b46138a" {
		panic("independent S00 vector mismatch")
	}
	deviceInput := proofInput{AccountID: account, SessionID: session, DeviceID: device, ProofID: proofID, Purpose: "revoke_devices", Nonce: nonce, RequestHash: doc.Requests[0].Hash, ExpiresAt: "2026-10-19T01:00:00.100Z"}
	doc.Proofs = append(doc.Proofs, proofVector{Name: "device-milliseconds", Kind: "device", Input: deviceInput})
	deviceInput.ExpiresAt = "2026-10-19T01:00:00Z"
	doc.Proofs = append(doc.Proofs, proofVector{Name: "device-whole-second", Kind: "device", Input: deviceInput})
	doc.Proofs = append(doc.Proofs, proofVector{Name: "resume-whole-second", Kind: "resume", Input: proofInput{AttemptID: operation, DeviceID: device, Nonce: nonce, ExpiresAt: "2026-10-19T01:00:00Z"}})
	for _, generation := range []string{"1", "9007199254740993", "9223372036854775807"} {
		doc.Proofs = append(doc.Proofs, proofVector{Name: "refresh-" + generation, Kind: "refresh", Input: proofInput{OperationID: operation, SessionID: session, Generation: generation, RefreshHash: doc.RefreshHash}})
	}
	for i := range doc.Proofs {
		v := &doc.Proofs[i]
		payload := payloadFor(*v)
		v.Payload = string(payload)
		v.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	}
	return doc
}

func payloadFor(v proofVector) []byte {
	i := v.Input
	var payload []byte
	var err error
	switch v.Kind {
	case "device":
		nonce, e := base64.RawURLEncoding.Strict().DecodeString(i.Nonce)
		must(e)
		hash, e := hex.DecodeString(i.RequestHash)
		must(e)
		expires, e := time.Parse(time.RFC3339Nano, i.ExpiresAt)
		must(e)
		payload, err = crypto.DeviceProofPayload(proofs.DeviceProof{AccountID: uuid.MustParse(i.AccountID), SessionID: uuid.MustParse(i.SessionID), DeviceID: uuid.MustParse(i.DeviceID), ProofID: uuid.MustParse(i.ProofID), Purpose: proofs.ProofPurpose(i.Purpose), Nonce: nonce, RequestHash: [32]byte(hash), ExpiresAt: expires})
	case "resume":
		nonce, e := base64.RawURLEncoding.Strict().DecodeString(i.Nonce)
		must(e)
		expires, e := time.Parse(time.RFC3339Nano, i.ExpiresAt)
		must(e)
		payload, err = crypto.ResumeProofPayload(proofs.ResumeProof{AttemptID: uuid.MustParse(i.AttemptID), DeviceID: uuid.MustParse(i.DeviceID), Nonce: nonce, ExpiresAt: expires})
	case "refresh":
		hash, e := hex.DecodeString(i.RefreshHash)
		must(e)
		generation, e := strconv.ParseInt(i.Generation, 10, 64)
		must(e)
		payload, err = crypto.RefreshProofPayload(proofs.RefreshProof{OperationID: uuid.MustParse(i.OperationID), SessionID: uuid.MustParse(i.SessionID), Generation: generation, RefreshHash: [32]byte(hash)})
	default:
		panic("unknown synthetic proof")
	}
	must(err)
	return payload
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	doc := fixture()
	if len(os.Args) != 2 {
		panic("use emit or verify")
	}
	if os.Args[1] == "emit" {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		must(encoder.Encode(doc))
		return
	}
	if os.Args[1] != "verify" {
		panic("unknown mode")
	}
	var actual vectorDocument
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	must(decoder.Decode(&actual))
	if actual.Version != doc.Version || actual.PublicKey != doc.PublicKey || actual.RefreshHash != doc.RefreshHash || len(actual.Proofs) != len(doc.Proofs) || len(actual.Requests) != len(doc.Requests) {
		panic("JS vector count/context mismatch")
	}
	public, err := base64.RawURLEncoding.Strict().DecodeString(doc.PublicKey)
	must(err)
	for n, v := range doc.Proofs {
		got := actual.Proofs[n]
		signature, err := base64.RawURLEncoding.Strict().DecodeString(got.Signature)
		must(err)
		if got.Name != v.Name || !bytes.Equal([]byte(got.Payload), payloadFor(v)) || got.Signature != v.Signature || !ed25519.Verify(public, payloadFor(v), signature) {
			panic("JS signed proof mismatch: " + v.Name)
		}
	}
	for n, v := range doc.Requests {
		if actual.Requests[n].Name != v.Name || actual.Requests[n].Hash != v.Hash || actual.Requests[n].CanonicalBody != v.CanonicalBody {
			panic("JS request mismatch: " + v.Name)
		}
	}
	fmt.Printf("PASS: Go verified %d JS signatures and %d canonical requests\n", len(doc.Proofs), len(doc.Requests))
}
