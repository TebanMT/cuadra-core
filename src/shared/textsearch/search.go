// Package textsearch keeps operator-facing searches consistent in SQLite,
// PostgreSQL and in-memory reports. It never changes stored names or identity.
package textsearch

import "strings"

// EscapeLike makes the free-text query a literal substring search. In
// particular, user-entered '%' and '_' are not treated as SQL wildcards. We
// use '!' as the explicit escape character in both databases so PostgreSQL's
// string-literal settings cannot change the query semantics.
func EscapeLike(s string) string {
	s = strings.ReplaceAll(s, `!`, `!!`)
	s = strings.ReplaceAll(s, `%`, `!%`)
	return strings.ReplaceAll(s, `_`, `!_`)
}

// Normalize deliberately makes Spanish accents insensitive and
// uses Unicode-aware Go lower-casing. The SQL expressions below apply the
// same finite transliteration to stored values before each database's LOWER,
// avoiding SQLite/PostgreSQL drift for the operator-facing search box.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	return strings.NewReplacer(
		"á", "a", "à", "a", "ä", "a", "â", "a",
		"é", "e", "è", "e", "ë", "e", "ê", "e",
		"í", "i", "ì", "i", "ï", "i", "î", "i",
		"ó", "o", "ò", "o", "ö", "o", "ô", "o",
		"ú", "u", "ù", "u", "ü", "u", "û", "u",
		"ñ", "n",
		"\u0300", "", "\u0301", "", "\u0302", "", "\u0303", "", "\u0308", "",
	).Replace(s)
}

// SQLiteColumn accepts only a developer-owned column expression, never user input.
// SQLite LOWER/NOCASE alone do not fold accented uppercase letters.
func SQLiteColumn(column string) string {
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
		{"\u0300", ""}, {"\u0301", ""}, {"\u0302", ""}, {"\u0303", ""}, {"\u0308", ""},
	} {
		expr = "REPLACE(" + expr + ",'" + pair[0] + "','" + pair[1] + "')"
	}
	return "LOWER(" + expr + ")"
}

// PostgresColumn uses the same Spanish folding even with a C locale.
// column must be a developer-owned expression.
func PostgresColumn(column string) string {
	return "LOWER(TRANSLATE(COALESCE(" + column + ",'')," +
		"'ÁÀÄÂáàäâÉÈËÊéèëêÍÌÏÎíìïîÓÒÖÔóòöôÚÙÜÛúùüûÑñ\u0300\u0301\u0302\u0303\u0308'," +
		"'AAAAaaaaEEEEeeeeIIIIiiiiOOOOooooUUUUuuuuNn'))"
}

const spanishUpper = "ÁÀÄÂÉÈËÊÍÌÏÎÓÒÖÔÚÙÜÛÑ"

// SQLiteLowerColumn folds case only, preserving accents for exact identifiers
// such as promotion codes. AÑO10 and ANO10 must remain distinct.
func SQLiteLowerColumn(column string) string {
	expr := "COALESCE(" + column + ",'')"
	for _, upper := range spanishUpper {
		expr = "REPLACE(" + expr + ",'" + string(upper) + "','" + strings.ToLower(string(upper)) + "')"
	}
	return "LOWER(" + expr + ")"
}

// PostgresLowerColumn is the case-only counterpart, independent of C locales.
// Both functions accept developer-owned column expressions, never user input.
func PostgresLowerColumn(column string) string {
	return "LOWER(TRANSLATE(COALESCE(" + column + ",''),'" + spanishUpper + "','" + strings.ToLower(spanishUpper) + "'))"
}
