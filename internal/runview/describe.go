package runview

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	safetext "github.com/bide-ai/bide/internal/printable"
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
	d, _, err := ReadDescriptionFile(path)
	return d, err
}

// ReadDescriptionFile is ReadDescription that also returns the SHA-256 of the file's bytes, in hex, which
// sha256sum on the same file prints: a result that rests on the file says which file it was.
func ReadDescriptionFile(path string) (*plan.RegistryDescription, string, error) {
	// A named pipe or a device would block the open (or the read) for ever, so the type is asked
	// before the file is opened, and again of what was opened (the path may have changed between the
	// two); the open itself does not wait for a writer where the system can say so.
	if fi, err := os.Stat(path); err != nil {
		return nil, "", err
	} else if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s: not a regular file", path)
	}
	f, err := openNoWait(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s: not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxDescriptionFile+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > MaxDescriptionFile {
		return nil, "", fmt.Errorf("%s: larger than %d bytes", path, MaxDescriptionFile)
	}
	d, err := DecodeDescription(b, path)
	sum := sha256.Sum256(b)
	return d, hex.EncodeToString(sum[:]), err
}

// DecodeDescription is ReadDescription for bytes already read; name is used in errors.
func DecodeDescription(b []byte, name string) (*plan.RegistryDescription, error) {
	if err := checkDepth(b); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var d plan.RegistryDescription
	if err := strictjson.Unmarshal(b, &d, nil); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return nil, fmt.Errorf("%s: data after the description", name)
		}
		return nil, fmt.Errorf("%s: %s", name, clipFileError(err.Error()))
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %s", name, clipFileError(err.Error()))
	}
	return &d, nil
}

// maxErrorText bounds the text of an error about a file's contents: a hostile file can make a
// decoder's message as long as the file.
const maxErrorText = 200

// clipFileError is s written out (safetext.Escape) and cut to maxErrorText bytes, with "..." when it was cut.
func clipFileError(s string) string {
	s = safetext.Escape(s)
	if len(s) <= maxErrorText {
		return s
	}
	cut := maxErrorText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// maxDescriptionDepth bounds how deeply a description file may nest. A description nests five levels at
// most; a decoder that recurses with the input (encoding/json without its v2 experiment, and the
// strict check before it) would otherwise spend memory in proportion to a hostile file's brackets.
const maxDescriptionDepth = 32

// checkDepth refuses b if its JSON nests deeper than maxDescriptionDepth, without parsing it: it
// counts brackets outside strings.
func checkDepth(b []byte) error {
	depth, inString, escaped := 0, false, false
	for _, c := range b {
		switch {
		case escaped:
			escaped = false
		case inString:
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			if depth++; depth > maxDescriptionDepth {
				return fmt.Errorf("nested more than %d levels deep", maxDescriptionDepth)
			}
		case c == ']' || c == '}':
			depth = max(depth-1, 0) // a stray closer is the decoder's to refuse; it must not buy depth
		}
	}
	return nil
}
