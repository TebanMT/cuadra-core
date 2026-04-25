package repositories

// normalizePage clamps page (≥1) and pageSize (1..200) — shared between the
// Postgres and SQLite implementations. Lives in a no-build-tag file so both
// compile against the same definition (avoids the duplicate-declaration error
// the test target hits when it builds with both `server` and `sidecar`).
func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 25
	}
	return page, pageSize
}
