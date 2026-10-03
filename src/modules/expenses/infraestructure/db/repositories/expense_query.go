package repositories

import "strings"

// escapeLike makes the free-text query a literal substring search. In
// particular, user-entered '%' and '_' are not treated as SQL wildcards. We
// use '!' as the explicit escape character in both databases so PostgreSQL's
// string-literal settings cannot change the query semantics.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `!`, `!!`)
	s = strings.ReplaceAll(s, `%`, `!%`)
	return strings.ReplaceAll(s, `_`, `!_`)
}

// normalizeExpenseSearch deliberately makes Spanish accents insensitive and
// uses Unicode-aware Go lower-casing. The SQL expressions below apply the
// same finite transliteration to stored values before each database's LOWER,
// avoiding SQLite/PostgreSQL drift for the operator-facing search box.
func normalizeExpenseSearch(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	return strings.NewReplacer(
		"á", "a", "à", "a", "ä", "a", "â", "a",
		"é", "e", "è", "e", "ë", "e", "ê", "e",
		"í", "i", "ì", "i", "ï", "i", "î", "i",
		"ó", "o", "ò", "o", "ö", "o", "ô", "o",
		"ú", "u", "ù", "u", "ü", "u", "û", "u",
		"ñ", "n",
	).Replace(s)
}

func sqliteNormalizedSearchColumn(column string) string {
	expr := "COALESCE(" + column + ",'')"
	for _, pair := range [][2]string{
		{"Á", "A"}, {"À", "A"}, {"Ä", "A"}, {"Â", "A"},
		{"á", "a"}, {"à", "a"}, {"ä", "a"}, {"â", "a"},
		{"É", "E"}, {"È", "E"}, {"Ë", "E"}, {"Ê", "E"},
		{"é", "e"}, {"è", "e"}, {"ë", "e"}, {"ê", "e"},
		{"Í", "I"}, {"Ì", "I"}, {"Ï", "I"}, {"Î", "I"},
		{"í", "i"}, {"ì", "i"}, {"ï", "i"}, {"î", "i"},
		{"Ó", "O"}, {"Ò", "O"}, {"Ö", "O"}, {"Ô", "O"},
		{"ó", "o"}, {"ò", "o"}, {"ö", "o"}, {"ô", "o"},
		{"Ú", "U"}, {"Ù", "U"}, {"Ü", "U"}, {"Û", "U"},
		{"ú", "u"}, {"ù", "u"}, {"ü", "u"}, {"û", "u"},
		{"Ñ", "N"}, {"ñ", "n"},
	} {
		expr = "REPLACE(" + expr + ",'" + pair[0] + "','" + pair[1] + "')"
	}
	return "LOWER(" + expr + ")"
}

func postgresNormalizedSearchColumn(column string) string {
	return "LOWER(TRANSLATE(COALESCE(" + column + ",'')," +
		"'ÁÀÄÂáàäâÉÈËÊéèëêÍÌÏÎíìïîÓÒÖÔóòöôÚÙÜÛúùüûÑñ'," +
		"'AAAAaaaaEEEEeeeeIIIIiiiiOOOOooooUUUUuuuuNn'))"
}
