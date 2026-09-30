// Package lib is in a nested module, which the same walk checks.
package lib

func Undocumented() {} // want `nested/lib Undocumented: exported func Undocumented has no doc comment`
