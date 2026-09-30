// Package model is a test fixture: packages a/model and b/model both declare model.Req, a pair of
// distinct types whose reflect.Type.String() is the same ("model.Req"), so plan's digest tests can
// show the digest tells them apart by their package path.
package model

// Req is the fixture type.
type Req struct {
	ID int `json:"id"`
}
