package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/recovery"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

const recoveryCompletePurpose = "recovery_complete"
const recoveryRejectedPurpose = "recovery_complete_rejected"
const recoveryPhoneRejectedPurpose = "recovery_complete_phone_unavailable"

type CompleteRecoveryInput struct {
	Binding            attempt.Binding
	OperationID        uuid.UUID
	RequestHash        [32]byte
	RecoveryKeyID      uuid.UUID
	RecoverySecret     string
	ConfirmPhoneChange bool
}

func (CompleteRecoveryInput) String() string     { return "recovery completion (credentials redacted)" }
func (v CompleteRecoveryInput) GoString() string { return v.String() }

type CompleteRecoveryDependencies struct {
	UoW             shared.UnitOfWork
	Attempts        repository.AuthAttemptRepository
	Identity        repository.RecoveryIdentityRepository
	Recovery        repository.RecoveryKeyRepository
	Sessions        repository.AuthSessionRepository
	Receipts        repository.AuthReceiptRepository
	Events          repository.SecurityEventRepository
	Tokens          security.AccessTokens
	RecoverySecrets security.RecoveryKeys
	Envelopes       security.Envelopes
	Budgets         budget.Repository
	Rules           budget.Rules
	Subjects        budget.SubjectHasher
	Now             func() time.Time
}
type CompleteRecovery struct{ dependencies CompleteRecoveryDependencies }

func NewCompleteRecovery(d CompleteRecoveryDependencies) (*CompleteRecovery, error) {
	if d.UoW == nil || d.Attempts == nil || d.Identity == nil || d.Recovery == nil || d.Sessions == nil || d.Receipts == nil || d.Events == nil || d.Tokens == nil || d.RecoverySecrets == nil || d.Envelopes == nil || d.Budgets == nil || d.Subjects == nil || d.Now == nil {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	// Reject phone rules, zero rules and incomplete recovery policy at wiring.
	if _, err := d.Rules.Plan(time.Unix(1, 0), budget.Check, map[budget.Scope][32]byte{budget.Recovery: {1}}); err != nil {
		return nil, personalErrors.ErrBudgetConfiguration
	}
	return &CompleteRecovery{dependencies: d}, nil
}

type recoveryAccessResponse struct {
	AttemptID     uuid.UUID         `json:"attempt_id"`
	ConsumedKeyID uuid.UUID         `json:"consumed_key_id"`
	Candidate     attempt.Candidate `json:"candidate"`
	Authorized    AuthorizedResult  `json:"authorized"`
}

func (recoveryAccessResponse) String() string     { return "recovery result (credentials redacted)" }
func (r recoveryAccessResponse) GoString() string { return r.String() }

func readRecoveryAttempt(ctx context.Context, uow shared.UnitOfWork, attempts repository.AuthAttemptRepository, binding attempt.Binding) (*attempt.AuthAttempt, error) {
	if binding.Validate() != nil {
		return nil, personalErrors.ErrInvalidAuthAttempt
	}
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return nil, err
	}
	q, err := uow.Query(shared.WithPersonalAttempt(ctx, binding.ID, binding.ContinuationHash))
	if err != nil {
		return nil, err
	}
	defer shared.CloseQuery(q)
	a, err := attempts.Get(q, binding)
	if err != nil {
		return nil, err
	}
	if a.Purpose != attempt.Recovery {
		return nil, personalErrors.ErrRecoveryKeyState
	}
	return a, nil
}

func (u *CompleteRecovery) Execute(ctx context.Context, in CompleteRecoveryInput) (AuthorizedResult, error) {
	if in.Binding.Validate() != nil || !deviceauthorization.ValidID(in.OperationID) || !deviceauthorization.ValidID(in.RecoveryKeyID) || in.RequestHash == [32]byte{} || len(in.RecoverySecret) < 32 || len(in.RecoverySecret) > 512 {
		return AuthorizedResult{}, personalErrors.ErrInvalidRecoveryKey
	}
	if err := shared.ValidatePersonalAnonymousScope(ctx); err != nil {
		return AuthorizedResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for retries := 0; retries < 3; retries++ {
		result, err := u.executeOnce(ctx, in)
		if !errors.Is(err, personalErrors.ErrRecoveryIdentityRetry) {
			return result, err
		}
	}
	return AuthorizedResult{}, shared.NewUnexpectedError(errors.New("la recuperación cambió; conserva el intento y vuelve a intentarlo"))
}

func (u *CompleteRecovery) executeOnce(ctx context.Context, in CompleteRecoveryInput) (AuthorizedResult, error) {
	d := u.dependencies
	a, err := readRecoveryAttempt(ctx, d.UoW, d.Attempts, in.Binding)
	if err != nil {
		return AuthorizedResult{}, err
	}
	scope := shared.WithPersonalAttempt(ctx, in.Binding.ID, in.Binding.ContinuationHash)
	if a.State == attempt.Consumed && a.AccountID != nil {
		scope = shared.WithPersonalAccount(scope, *a.AccountID)
	}
	var result AuthorizedResult
	var rejection error
	err = d.UoW.Command(scope, func(tx shared.Transaction) error {
		current, err := d.Attempts.Get(tx, in.Binding)
		if err != nil {
			return err
		}
		failed, err := d.Receipts.Get(tx, receipt.Owner{AttemptID: in.Binding.ID}, in.OperationID)
		if err == nil {
			rejection, err = recoveryRejectionReplay(failed, in.RequestHash)
			return err
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		if current.State == attempt.Consumed {
			if current.AccountID == nil || a.State != attempt.Consumed {
				return personalErrors.ErrRecoveryIdentityRetry
			}
			if current.ResultReference == nil || *current.ResultReference != in.OperationID {
				return personalErrors.ErrRecoveryKeyState
			}
			result, err = readRecoveryResult(recoveryReaderDependencies(d), tx, current, &in)
			return err
		}
		if err = recovery.ValidateVerifiedAttempt(current, d.Now()); err != nil {
			return err
		}
		if err = checkRecoveryOTPReceipt(d.Receipts, tx, current); err != nil {
			return err
		}
		subject, err := d.Subjects.Hash(budget.Recovery, in.RecoveryKeyID.String())
		if err != nil {
			return err
		}
		plan, err := d.Rules.Plan(d.Now(), budget.Check, map[budget.Scope][32]byte{budget.Recovery: subject})
		if err != nil {
			return err
		}
		if err = d.Budgets.Reserve(tx, plan); err != nil {
			if errors.Is(err, personalErrors.ErrBudgetExceeded) || errors.Is(err, personalErrors.ErrBudgetBlocked) {
				// A winner may have used the last slot while this retry was waiting.
				// Roll back this reservation and re-enter the durable receipt path.
				if _, receiptErr := d.Receipts.Get(tx, receipt.Owner{AttemptID: in.Binding.ID}, in.OperationID); receiptErr == nil {
					return personalErrors.ErrRecoveryIdentityRetry
				} else if !errors.Is(receiptErr, personalErrors.ErrReceiptNotFound) {
					return receiptErr
				}
				latest, readErr := d.Attempts.Get(tx, in.Binding)
				if readErr != nil {
					return readErr
				}
				if latest.State == attempt.Consumed {
					return personalErrors.ErrRecoveryIdentityRetry
				}
			}
			return err
		}
		// Identical concurrent retries wait on the recovery budget subject. Recheck
		// their durable receipt before charging a second check or touching identity.
		failed, err = d.Receipts.Get(tx, receipt.Owner{AttemptID: in.Binding.ID}, in.OperationID)
		if err == nil {
			return personalErrors.ErrRecoveryIdentityRetry
		}
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			return err
		}
		current, err = d.Attempts.Get(tx, in.Binding)
		if err != nil {
			return err
		}
		if current.State == attempt.Consumed {
			return personalErrors.ErrRecoveryIdentityRetry
		}
		bound, err := d.Identity.ResolveVerifiedRecovery(tx, in.Binding, in.RecoveryKeyID)
		if err != nil {
			if errors.Is(err, personalErrors.ErrRecoveryKeyState) {
				rejection = personalErrors.ErrRecoveryKeyState
				return storeRecoveryRejection(d, tx, in, rejection)
			}
			return err
		}
		key, err := d.Recovery.Get(tx, bound.AccountID, in.RecoveryKeyID)
		if errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
			rejection = personalErrors.ErrRecoveryKeyState
			return storeRecoveryRejection(d, tx, in, rejection)
		}
		if err != nil {
			return err
		}
		if !key.Live() || d.RecoverySecrets.Verify(in.RecoverySecret, key.Verifier) != nil {
			rejection = personalErrors.ErrRecoveryKeyState
			return storeRecoveryRejection(d, tx, in, rejection)
		}
		if !bound.PhoneAvailable {
			rejection = personalErrors.ErrRecoveryPhoneUnavailable
			return storeRecoveryRejection(d, tx, in, rejection)
		}
		if bound.PhoneChangeRequired && !in.ConfirmPhoneChange {
			rejection = personalErrors.ErrRecoveryKeyState
			return storeRecoveryRejection(d, tx, in, rejection)
		}
		current, err = d.Attempts.GetForUpdate(tx, in.Binding)
		if err != nil {
			return err
		}
		now := d.Now().UTC().Truncate(time.Millisecond)
		if err = recovery.ValidateVerifiedAttempt(current, now); err != nil {
			return err
		}
		generation, err := d.Recovery.NextGeneration(tx, bound.AccountID)
		if err != nil {
			return err
		}
		// From the first authority write onward, every error propagates and rolls
		// back budget, kit, phone, sessions, challenge, audit and response together.
		if err = key.Consume(now); err != nil {
			return err
		}
		if err = d.Recovery.SaveState(tx, bound.AccountID, key); err != nil {
			return err
		}
		if err = d.Identity.ApplyRecovery(tx, in.Binding, bound.AccountID, key.ID, in.ConfirmPhoneChange, now); err != nil {
			return err
		}
		result, err = createRecoveryAuthority(d, tx, current, bound.AccountID, generation, now)
		if err != nil {
			return err
		}
		revision := current.Revision
		if err = current.BindAccount(bound.AccountID, now); err != nil {
			return err
		}
		if err = current.Consume(in.OperationID, now); err != nil {
			return err
		}
		if err = d.Attempts.Save(tx, in.Binding, current, revision); err != nil {
			return err
		}
		r, err := receipt.New(in.OperationID, receipt.Owner{AccountID: bound.AccountID, AttemptID: current.ID}, recoveryCompletePurpose, in.RequestHash, now)
		if err != nil {
			return err
		}
		plain, err := json.Marshal(recoveryAccessResponse{AttemptID: current.ID, ConsumedKeyID: key.ID, Candidate: current.Candidate, Authorized: result})
		if err != nil {
			return personalErrors.ErrInvalidReceipt
		}
		defer clear(plain)
		envelope, err := d.Envelopes.SealReceipt(r.Context(), plain)
		if err != nil {
			return err
		}
		if err = r.SetResponse(envelope, now.Add(10*time.Minute), uuid.MustParse(result.Session.SessionID)); err != nil {
			return err
		}
		if err = d.Receipts.Create(tx, r); err != nil {
			return err
		}
		return d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: bound.AccountID, ActorDeviceID: current.Candidate.ID, OperationID: in.OperationID, Type: "account_recovered", OccurredAt: now})
	})
	if err != nil {
		return AuthorizedResult{}, err
	}
	if rejection != nil {
		return AuthorizedResult{}, rejection
	}
	return result, nil
}

func checkRecoveryOTPReceipt(receipts repository.AuthReceiptRepository, tx shared.Transaction, a *attempt.AuthAttempt) error {
	if a.ProviderOperation == nil {
		return personalErrors.ErrRecoveryKeyState
	}
	r, err := receipts.Get(tx, receipt.Owner{AttemptID: a.ID}, a.ProviderOperation.ID)
	if err != nil {
		return err
	}
	if err = r.Match("auth_check", a.ProviderOperation.RequestHash); err != nil {
		return personalErrors.ErrRecoveryKeyState
	}
	if r.CompletedAt.Before(a.ProviderOperation.StartedAt) {
		return personalErrors.ErrRecoveryKeyState
	}
	return nil
}
func storeRecoveryRejection(d CompleteRecoveryDependencies, tx shared.Transaction, in CompleteRecoveryInput, why error) error {
	purpose := recoveryRejectedPurpose
	if errors.Is(why, personalErrors.ErrRecoveryPhoneUnavailable) {
		purpose = recoveryPhoneRejectedPurpose
	}
	r, err := receipt.New(in.OperationID, receipt.Owner{AttemptID: in.Binding.ID}, purpose, in.RequestHash, d.Now())
	if err != nil {
		return err
	}
	return d.Receipts.Create(tx, r)
}
func recoveryRejectionReplay(r *receipt.Receipt, hash [32]byte) (error, error) {
	if r.Purpose != recoveryRejectedPurpose && r.Purpose != recoveryPhoneRejectedPurpose {
		return nil, personalErrors.ErrIdempotencyMismatch
	}
	if err := r.Match(r.Purpose, hash); err != nil {
		return nil, err
	}
	if r.ResultReference != uuid.Nil || len(r.Response.Ciphertext) != 0 {
		return nil, personalErrors.ErrInvalidReceipt
	}
	if r.Purpose == recoveryPhoneRejectedPurpose {
		return personalErrors.ErrRecoveryPhoneUnavailable, nil
	}
	return personalErrors.ErrRecoveryKeyState, nil
}
func createRecoveryAuthority(d CompleteRecoveryDependencies, tx shared.Transaction, a *attempt.AuthAttempt, accountID uuid.UUID, generation int64, now time.Time) (AuthorizedResult, error) {
	newDevice, err := device.NewAuthorizedDevice(accountID, a.Candidate, device.Recovery, now)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if err = d.Sessions.CreateDevice(tx, accountID, newDevice); err != nil {
		return AuthorizedResult{}, err
	}
	refresh, err := randomAccessSecret()
	if err != nil {
		return AuthorizedResult{}, err
	}
	ticket, err := randomAccessSecret()
	if err != nil {
		return AuthorizedResult{}, err
	}
	s, err := session.NewSession(accountID, a.Candidate.ID, uuid.New(), sha256.Sum256([]byte(refresh)), uuid.New(), sha256.Sum256([]byte(ticket)), now)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if err = d.Sessions.CreateSession(tx, accountID, s); err != nil {
		return AuthorizedResult{}, err
	}
	access, claims, err := d.Tokens.Issue(accountID, s.ID, s.DeviceID, now)
	if err != nil {
		return AuthorizedResult{}, err
	}
	secret, err := randomAccessSecret()
	if err != nil {
		return AuthorizedResult{}, err
	}
	verifier, err := d.RecoverySecrets.CreateVerifier(secret)
	if err != nil {
		return AuthorizedResult{}, err
	}
	key, err := recovery.NewKey(accountID, uuid.New(), generation, verifier, now)
	if err != nil {
		return AuthorizedResult{}, err
	}
	if err = d.Recovery.Create(tx, accountID, key); err != nil {
		return AuthorizedResult{}, err
	}
	return AuthorizedResult{Status: "authorized", Session: SessionTokens{AccountID: accountID.String(), DeviceID: s.DeviceID.String(), SessionID: s.ID.String(), AccessToken: access, AccessExpiresAt: accessTimestamp(claims.ExpiresAt), RefreshToken: refresh, RefreshExpiresAt: accessTimestamp(s.ExpiresAt), Generation: "1", RevocationTicket: ticket}, RecoveryKit: &RecoveryKit{ID: key.ID.String(), Secret: secret, Generation: strconv.FormatInt(generation, 10)}}, nil
}
