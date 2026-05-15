package repositories

// normalizePage clamps page (≥1) y pageSize (1..200) — shared entre las
// implementaciones Postgres y SQLite del repo de expenses.
func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	return page, pageSize
}
