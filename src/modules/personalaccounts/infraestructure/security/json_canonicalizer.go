package security

// JSONCanonicalizer is the protocol's strict UTF-8/duplicate-key/integer codec.
// Commands hash the complete command, independently of its HTTP endpoint, so a
// later push and a direct retry share the same durable operation identity.
type JSONCanonicalizer struct{}

func (JSONCanonicalizer) Canonicalize(raw []byte) ([]byte, error) {
	value, err := parseJSON(raw)
	if err != nil {
		return nil, err
	}
	return canonical(value)
}
