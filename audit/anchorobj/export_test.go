package anchorobj

// SetAfterTypeCheck sets the hook LoadDir runs between a file's type check and its open.
func SetAfterTypeCheck(f func()) { afterTypeCheck = f }
