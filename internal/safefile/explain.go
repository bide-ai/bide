package safefile

import "errors"

const (
	// MaxKey is the most a file holding one public key may have: a key is under 100 bytes as text,
	// and a few hundred in a PEM wrapper.
	MaxKey = 1 << 20
	// MaxDocument is the most a small document may have (a policy, a receipt with its certificate
	// chain, a CA bundle, a parameters file): the largest realistic one is a few hundred KiB, a
	// receipt carrying the eight 16 KiB certificates a chain may hold being about 175 KiB.
	MaxDocument = 16 << 20
)

// FlagError is a refusal from Read or ReadOrStdin with the flag that named the file, so that the
// message says which flag, what its limit is and, where the flag takes it, the "-" to use instead.
type FlagError struct {
	Flag    string
	StdinOK bool
	Err     error
}

func (e *FlagError) Error() string {
	var re *RefusedError
	if !errors.As(e.Err, &re) {
		return e.Err.Error()
	}
	switch {
	case re.Kind == TooLarge:
		return e.Err.Error() + " (the limit for " + e.Flag + ")"
	case e.StdinOK:
		return e.Err.Error() + ": a named pipe, a device or a process substitution is not read; pass - to read " + e.Flag + " from standard input"
	}
	return e.Err.Error()
}

func (e *FlagError) Unwrap() error { return e.Err }

// Explain wraps a refusal of the file named by flag in a FlagError. Any other error, and nil, come
// back as they are. stdinOK says that the flag takes "-" for standard input.
func Explain(flag string, err error, stdinOK bool) error {
	var re *RefusedError
	if err == nil || !errors.As(err, &re) {
		return err
	}
	return &FlagError{Flag: flag, StdinOK: stdinOK, Err: err}
}
