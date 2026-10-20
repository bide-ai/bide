package runview

import (
	"bytes"
	"io"
	"io/fs"
	"time"

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

// AssetsWith is Assets with the shipped themes and extra, a custom theme, in the page's style.css,
// and def, when it is not empty, the theme a reader starts in (the name of a shipped theme or of
// one of extra). It panics on a def that names no theme, as theme.CSSDefault does.
func AssetsWith(def string, extra ...*theme.Theme) fs.FS {
	base := rawAssets()
	raw, err := fs.ReadFile(base, "style.css")
	if err != nil {
		panic(err) // style.css is in the embedded directory
	}
	css := theme.CSSDefault(append(theme.Shipped(), extra...), def)
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
