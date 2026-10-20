package safefile

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
)

// Stdin is the stream ReadOrStdin reads for "-". Tests replace it.
var Stdin io.Reader = os.Stdin

var stdin struct {
	mu    sync.Mutex
	flag  string
	data  []byte
	err   error
	taken bool
}

// ResetStdin forgets that standard input was read, for tests that run a command more than once in
// one process.
func ResetStdin() {
	stdin.mu.Lock()
	defer stdin.mu.Unlock()
	stdin.flag, stdin.data, stdin.err, stdin.taken = "", nil, nil, false
}

// ReadOrStdin is Read, except that the path "-" reads standard input (at most max bytes, refused
// with a TooLarge RefusedError named "standard input" when longer). It is how a flag takes a secret
// or a policy that a secrets tool writes to a pipe: a path that names a pipe is refused by Read, so
// the flag takes "-" instead. Standard input can be read by one flag only: flag names the flag
// reading it, a second read by the same flag returns the same bytes (a flag read at two places), and
// a read by another flag is an error naming both, never an empty or partial document. To read a file
// named "-", write "./-".
func ReadOrStdin(flag, path string, max int) ([]byte, error) {
	if path != "-" {
		return Read(path, max, nil)
	}
	stdin.mu.Lock()
	defer stdin.mu.Unlock()
	if stdin.taken {
		if stdin.flag != flag {
			return nil, fmt.Errorf("%s - and %s - both read standard input: only one flag can", stdin.flag, flag)
		}
		return stdin.data, stdin.err
	}
	stdin.taken, stdin.flag = true, flag
	limit := int64(max)
	if limit < math.MaxInt64 {
		limit++
	}
	b, err := io.ReadAll(io.LimitReader(Stdin, limit))
	switch {
	case err != nil:
		stdin.err = err
	case len(b) > max:
		stdin.err = &RefusedError{Path: "standard input", Kind: TooLarge, Max: max}
	case len(b) == 0:
		stdin.err = errors.New("standard input is empty")
	default:
		stdin.data = b
	}
	return stdin.data, stdin.err
}
