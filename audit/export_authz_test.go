package audit

// SignLiftAuthorizationForTest re-signs a (after a test changed a field of it) with s, so a test can
// build an authorization that is internally consistent and wrong in one respect.
func SignLiftAuthorizationForTest(a LiftAuthorization, s Signer) (LiftAuthorization, error) {
	m, err := a.signedBytes()
	if err != nil {
		return a, err
	}
	a.Signature, err = s.Sign(m)
	return a, err
}
