// Package ssocerts is the worked recipe for approver certificates issued after a single sign-on login: a
// step-ca instance with an OIDC provisioner and a certificate template that issues exactly what a bide
// IdentityPolicy requires, and an end-to-end test that runs it against containers. The files are in ca,
// idp and scripts; the guide is docs/guides/sso-certificates.md.
package ssocerts
