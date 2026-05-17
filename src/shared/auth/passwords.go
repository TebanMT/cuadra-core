package auth

import (
	"crypto/rand"
	"errors"
	"math/big"
)

// GeneratePassword returns a temp password (UC-006 DA-6.1): 6 chars, charset
// excludes 0/O/1/l/I to make it readable when shared verbally over WhatsApp.
const tempPasswordCharset = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
const tempPasswordLength = 6

func GenerateTempPassword() (string, error) {
	out := make([]byte, tempPasswordLength)
	max := big.NewInt(int64(len(tempPasswordCharset)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = tempPasswordCharset[n.Int64()]
	}
	return string(out), nil
}

// GenerateTempPIN returns a 4-digit numeric PIN used as the operator's
// reception credential. CreateOperator / RotateOperatorPIN call this,
// hash the result with HashPIN, and ship the plaintext to the owner /
// WhatsApp once. We deliberately don't filter "weak" PINs like "1234"
// — the desktop is a kiosk in a small gym, the owner controls who is
// near the keypad, and banning obvious PINs just pushes owners to write
// the PIN on a sticky note (see ValidatePIN's comment in the user
// domain for the same rationale).
func GenerateTempPIN() (string, error) {
	return GenerateOTPCode(4)
}

// GenerateOTPCode returns a numeric OTP code of `digits` characters. UC-010
// uses 6 digits.
var ErrInvalidOTPLength = errors.New("invalid OTP length")

func GenerateOTPCode(digits int) (string, error) {
	if digits < 4 || digits > 10 {
		return "", ErrInvalidOTPLength
	}
	out := make([]byte, digits)
	max := big.NewInt(10)
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = byte('0' + n.Int64())
	}
	return string(out), nil
}
