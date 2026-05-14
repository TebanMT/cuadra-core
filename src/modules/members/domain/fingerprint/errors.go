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
	ErrFingerprintCollision  = errors.New("esta huella ya está registrada para otro socio")
	ErrEncryptionFailed      = errors.New("no se pudo cifrar la huella; revisa la configuración del gimnasio")
	ErrDecryptionFailed      = errors.New("no se pudo descifrar una huella; pasa con recepción")
	ErrConsentRequired       = errors.New("falta el consentimiento del socio para registrar la huella")
)

// CollisionThreshold (stricter than the check-in default 0.7) gates the
// 1:N pre-enrollment match. Asymmetry is intentional: at enrollment we
// decide "this finger IS the same person" and want minimal false positives
// — blocking a legitimate operator is costlier than letting a rare duplicate
// slip through. At check-in we identify the SAME person across captures,
// where natural variation justifies the more permissive threshold.
const CollisionThreshold = 0.85
