package themeaccess

import (
	"fmt"
	"io"
)

// ReadLimited reads the theme file at path: a regular file of at most max bytes. A named pipe or a
// device is refused without waiting for a writer (the open does not block on unix), and a file over
// max is refused without reading the rest of it, so a path named by mistake, or swapped after it
// was checked, cannot hang the caller or exhaust its memory.
func ReadLimited(path string, max int) ([]byte, error) {
	f, err := openNoWait(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, fmt.Errorf("%s: theme file is over the %d byte limit", path, max)
	}
	return data, nil
}
