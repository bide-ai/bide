package rfc3161

import "encoding/asn1"

var (
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSTInfo       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidAttrContent   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidAttrDigest    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidAttrSigCertV2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}

	oidSHA1   = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidMD5    = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 5}
	oidSHA224 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 4}
	oidSHA256 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidSHA384 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}

	oidRSAEncryption  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidMD5WithRSA     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 4}
	oidSHA1WithRSA    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 5}
	oidSHA256WithRSA  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidSHA384WithRSA  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 12}
	oidSHA512WithRSA  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	oidSHA224WithRSA  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 14}
	oidRSAPSS         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 10}
	oidMGF1           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}
	oidECDSASHA1      = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 1}
	oidECDSASHA224    = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 1}
	oidECDSASHA256    = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSASHA384    = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSASHA512    = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
	oidEd25519        = asn1.ObjectIdentifier{1, 3, 101, 112}
	oidExtKeyUsage    = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidKPTimeStamping = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}
)
