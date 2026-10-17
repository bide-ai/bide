{{- /*
  The template of the operator's JWK provisioner, which can revoke any certificate of this CA (step-ca lets
  any JWK provisioner revoke any certificate) and issues only an ordinary client certificate for
  administration. It asserts no approver EKU and no policy OID, so a certificate from it is never accepted for
  an approver; keep its key (and password) with the CA operators, in a KMS or an HSM.
*/ -}}
{"subject": {{ toJson .Subject }}, "sans": {{ toJson .SANs }}, "keyUsage": ["digitalSignature"], "extKeyUsage": ["clientAuth"]}
