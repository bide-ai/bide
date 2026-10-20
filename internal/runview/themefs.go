package runview

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/bide-ai/bide/internal/themeaccess"
	"github.com/bide-ai/bide/theme"
)

// themedAssets is the assets directory with style.css replaced by the generated theme stylesheet
// followed by the file itself. The pages already load style.css first and use only the custom
// properties it defines, so every page (and the editor, which serves this file from here) gets its
// tokens from the checked themes without a change to any page.
type themedAssets struct {
	fs.FS
	style []byte
}

// ThemeSet is the themes a page offers: Extra, custom themes beside the shipped light, dark and
// high-contrast; Default, the name of the theme a reader starts in (empty: the system's setting
// picks light or dark); and Lock, the name of the one theme the page has (no switch, no remembered
// choice). The system's request for more contrast or forced colors wins over a Default and a Lock.
type ThemeSet struct {
	Extra   []*theme.Theme
	Default string
	Lock    string
}

// Validate reports why the set cannot be served: a theme that is nil or not from theme.Parse,
// Load, Shipped or Lookup, two themes with one name (a custom theme named like a shipped one
// included), a Default or Lock that names no theme, or a Default and a Lock that differ.
func (s ThemeSet) Validate() error {
	seen := map[string]bool{}
	for _, t := range theme.Shipped() {
		seen[t.Name()] = true
	}
	for _, t := range s.Extra {
		switch {
		case t == nil || t.Name() == "":
			return errors.New("a theme is nil or did not come from theme.Parse, Load, Shipped or Lookup")
		case seen[t.Name()]:
			return fmt.Errorf("two themes are named %q", t.Name())
		}
		seen[t.Name()] = true
	}
	if s.Default != "" && !seen[s.Default] {
		return fmt.Errorf("the default theme %q names no theme", s.Default)
	}
	if s.Lock != "" && !seen[s.Lock] {
		return fmt.Errorf("the locked theme %q names no theme", s.Lock)
	}
	if s.Default != "" && s.Lock != "" && s.Default != s.Lock {
		return fmt.Errorf("the default theme %q and the locked theme %q differ: a lock is the only theme the page has", s.Default, s.Lock)
	}
	return nil
}

// AssetsWith is Assets with the shipped themes and the set's custom themes in the page's style.css,
// the set's Default as the theme a reader starts in and its Lock as the only theme. It panics on a
// set that does not Validate.
func AssetsWith(s ThemeSet) fs.FS {
	if err := s.Validate(); err != nil {
		panic("runview: " + err.Error())
	}
	base := rawAssets()
	raw, err := fs.ReadFile(base, "style.css")
	if err != nil {
		panic(err) // style.css is in the embedded directory
	}
	all := append(theme.Shipped(), s.Extra...)
	var css string
	if s.Lock != "" {
		css = themeaccess.CSSLocked(all, s.Lock)
	} else {
		css = themeaccess.CSSDefault(all, s.Default)
	}
	return themedAssets{FS: base, style: append([]byte(css), raw...)}
}

// Open serves the composed style.css and every other file as embedded.
func (a themedAssets) Open(name string) (fs.File, error) {
	if name != "style.css" {
		return a.FS.Open(name)
	}
	return &memFile{Reader: bytes.NewReader(a.style), info: memInfo{name: name, size: int64(len(a.style))}}, nil
}

// memFile is an in-memory file that can seek, which net/http needs to serve a size and ranges.
type memFile struct {
	*bytes.Reader
	info memInfo
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error               { return nil }

var _ io.ReadSeeker = (*memFile)(nil)

type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
