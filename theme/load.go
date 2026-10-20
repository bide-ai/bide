package theme

import (
	"errors"

	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/themeaccess"
)

// Load reads and checks a custom theme file: Parse on the file's bytes. The path must name a regular
// file of at most MaxSize bytes; a named pipe or a device is refused, not waited on.
// The report is returned with an error from a failed check as well as with success.
func Load(path string) (*Theme, Report, error) {
	data, err := safefile.Read(path, MaxSize, nil)
	if err != nil {
		return nil, Report{}, errors.New(themeaccess.Escape(err.Error()))
	}
	return Parse(data)
}
