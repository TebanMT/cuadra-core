package fingerprint

import "errors"

// Sentinel errors for the MemberFingerprint aggregate. Surfaced (wrapped in
// CustomError by the use case) to the operator at UC-028 / UC-029.
var (
	ErrInvalidIdentifiers    = errors.New("identificadores inválidos para huella")
	ErrEmptyTemplate         = errors.New("la huella capturada está vacía")
	ErrQualityBelowFloor     = errors.New("la calidad de la huella es muy baja; vuelve a capturarla")
	ErrFingerprintNotFound   = errors.New("este socio no tiene huella registrada")
	ErrFingerprintAlreadySet = errors.New("este socio ya tiene huella registrada (borra la anterior antes de registrar una nueva)")
	ErrTooManyCaptures       = errors.New("se permiten máximo 3 capturas por registro de huella")
	ErrFingerprintCollision  = errors.New("esta huella ya está registrada para otro socio")
	ErrEncryptionFailed      = errors.New("no se pudo cifrar la huella; revisa la configuración del gimnasio")
	ErrDecryptionFailed      = errors.New("no se pudo descifrar una huella; pasa con recepción")
	ErrConsentRequired       = errors.New("falta el consentimiento del socio para registrar la huella")
)

// CollisionThreshold gates the 1:N pre-enrollment match. Like the check-in
// threshold it is a raw bozorth3 score (NOT a 0-1 ratio): Identify compares
// the matcher's integer score directly against it. Set stricter (higher)
// than the check-in default — at enrollment we decide "this finger IS the
// same person" and want minimal false collisions (wrongly blocking a legit
// new socio is costlier than letting a rare duplicate slip through).
// bozorth3 scores the same finger ~55 and unrelated fingers 3-7 (reader
// validation 2026-05-16), so 50 catches a genuine duplicate without
// false-flagging strangers.
const CollisionThreshold = 50
