// Package domain — bounded context `checkins`.
//
// Implemented in Sesión 5. The aggregate (Checkin) records ONE access
// decision per attempt. The decision itself comes from the members BC's
// AccessStatusEvaluator — checkins doesn't re-implement that logic, it just
// persists the verdict + the method (huella / manual / PIN) + override flag.
//
// Subpackages:
//
//	checkin     — Checkin aggregate + factories (NewFingerprintCheckin etc.)
//	errors      — sentinel errors for UC-029, UC-030, UC-032 + DA-29.2
//	repository  — persistence contract (impls live under infraestructure/)
//
// See CUADRA-USE-CASES.md UC-029..UC-032 for the full flows.
package domain
