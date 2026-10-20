package theme

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bide-ai/bide/internal/themeaccess"
)

// Load reads and checks a custom theme file: Parse on the file's bytes, read with the size limit.
// The report is returned with an error from a failed check as well as with success.
func Load(path string) (*Theme, Report, error) {
	data, err := readLimited(path)
	if err != nil {
		return nil, Report{}, errors.New(themeaccess.Escape(err.Error()))
	}
	return Parse(data)
}

// readLimited reads a theme file, refusing one over MaxSize without reading the rest of it, so a
// device or a huge file named by mistake cannot exhaust memory.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("%s: theme file is over the %d byte limit", path, MaxSize)
	}
	return data, nil
}
