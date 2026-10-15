package rfc3161

// Verify checks token against exp and pol. A refused token yields a *Refusal; an unusable exp or
// pol yields an error wrapping ErrConfig.
func Verify(token []byte, exp Expected, pol Policy) (*Result, error) {
	return nil, refuse(ReasonMalformed, "not implemented")
}
