package runview

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bide-ai/bide/internal/strictjson"
	"github.com/bide-ai/bide/plan"
)

// MaxDescriptionFile caps a description file: it is read whole, and a program with thousands of
// blocks describes itself in far less.
const MaxDescriptionFile = 1 << 20

// ReadDescription reads a plan.RegistryDescription from the file at path (what a program writes
// with json.Marshal of Registry.Describe). The file must be a regular file of at most
// MaxDescriptionFile bytes holding one JSON value with no field the description does not have,
// and the description must be valid (RegistryDescription.Validate). A description is untrusted
// data: this is the only way the viewer and the editor read one, and it is read once.
func ReadDescription(path string) (*plan.RegistryDescription, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxDescriptionFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxDescriptionFile {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, MaxDescriptionFile)
	}
	return DecodeDescription(b, path)
}

// DecodeDescription is ReadDescription for bytes already read; name is used in errors.
func DecodeDescription(b []byte, name string) (*plan.RegistryDescription, error) {
	var d plan.RegistryDescription
	if err := strictjson.Unmarshal(b, &d, nil); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return nil, fmt.Errorf("%s: data after the description", name)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &d, nil
}
