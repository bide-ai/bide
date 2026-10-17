Section: Added

audit.CertIdentity now reports the certificate rules a request for a certificate must ask for: IDKind, EKU, AllowsEmailProtectionEKU, PolicyOID and MaxLeafLifetime (the effective value, 24 hours when the policy set none). They are read-only (EKU returns a copy) and change no verdict; they let a tool such as a certificate request command name what the gate will look for.
