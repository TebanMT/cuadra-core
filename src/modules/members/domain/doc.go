// Package domain — bounded context `members`.
//
// Aggregates:
//   - Member          (member/) — UC-012..UC-017, UC-032, UC-035
//   - Membership      (membership/) — UC-018 renewal logic, UC-017 adjustments
//   - MembershipType  (membership_type/) — UC-011
//
// Domain services:
//   - access.AccessStatusEvaluator — consumed by checkins BC (Sesión 5)
//
// All persistence is fronted by the interfaces in repository/.
package domain
