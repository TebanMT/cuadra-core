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
	ErrEncryptionFailed      = errors.New("no se pudo cifrar la huella; revisa la configuración del gimnasio")
	ErrDecryptionFailed      = errors.New("no se pudo descifrar una huella; pasa con recepción")
	ErrConsentRequired       = errors.New("falta el consentimiento del socio para registrar la huella")
)
