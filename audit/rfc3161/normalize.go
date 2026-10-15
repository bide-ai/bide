package rfc3161

import (
	"errors"
	"fmt"
)

// Normalize returns token with its certificates field reduced to the certificates of the accepted
// path that the token carries (res.Chain, in the token's order); every other certificate is
// stripped. Certificates are not signed, so the result verifies exactly as token does and attests the
// same; a kit that stores tokens stores these minimal bytes. If the token carries only used
// certificates the bytes are returned unchanged, and a token whose path comes wholly from the
// policy loses its certificates field.
//
// Two encodings of one attestation are distinct byte strings: anything keyed on token bytes (a
// digest, a set) must key on the normalized form, or on the attested fields, not on the received
// bytes. res must be the result of Verify on this token.
func Normalize(token []byte, res *Result) ([]byte, error) {
	if res == nil || res.Signer == nil {
		return nil, fmt.Errorf("%w: Normalize needs the result of Verify", ErrConfig)
	}
	ci, rest, r := parseNode(token)
	if r != nil || len(rest) != 0 || !isSeq(ci) {
		return nil, errors.New("rfc3161: Normalize: not a token that Verify accepted")
	}
	cs, r := kids(ci)
	if r != nil || len(cs) != 2 || !isCtx(cs[1], 0, true) {
		return nil, errors.New("rfc3161: Normalize: not a token that Verify accepted")
	}
	sdn, r := only(cs[1], "content")
	if r != nil || !isSeq(sdn) {
		return nil, errors.New("rfc3161: Normalize: not a token that Verify accepted")
	}
	sd, r := kids(sdn)
	if r != nil {
		return nil, errors.New("rfc3161: Normalize: not a token that Verify accepted")
	}
	used := map[string]bool{}
	for _, c := range res.Chain {
		used[string(c.Raw)] = true
	}
	var body []byte
	changed := false
	for _, n := range sd {
		if !isCtx(n, 0, true) {
			body = append(body, n.FullBytes...)
			continue
		}
		certs, r := kids(n)
		if r != nil {
			return nil, errors.New("rfc3161: Normalize: not a token that Verify accepted")
		}
		var keep []byte
		kept := map[string]bool{}
		for _, c := range certs {
			if used[string(c.FullBytes)] && !kept[string(c.FullBytes)] {
				kept[string(c.FullBytes)] = true
				keep = append(keep, c.FullBytes...)
			} else {
				changed = true
			}
		}
		if len(keep) > 0 {
			body = append(body, derWrap(0xA0, keep)...)
		}
	}
	if !changed {
		return append([]byte(nil), token...), nil
	}
	sdOut := derWrap(0x30, body)
	return derWrap(0x30, append(append([]byte(nil), cs[0].FullBytes...), derWrap(0xA0, sdOut)...)), nil
}

// derWrap encodes a single-byte tag and a definite DER length around body.
func derWrap(tag byte, body []byte) []byte {
	out := []byte{tag}
	n := len(body)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	default:
		var l []byte
		for v := n; v > 0; v >>= 8 {
			l = append([]byte{byte(v)}, l...)
		}
		out = append(out, 0x80|byte(len(l)))
		out = append(out, l...)
	}
	return append(out, body...)
}
