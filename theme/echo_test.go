package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Load's error names the file and the problem but does not repeat a very long name from
// the file whole, nor write an escape sequence from it raw.
func TestLoad_ErrorDoesNotEchoALongNameOrEscape(t *testing.T) {
	dir := t.TempDir()
	for name, key := range map[string]string{
		"long name": "HOSTSECRET-" + strings.Repeat("k", 500),
		"escape":    `\u001b[31mred`,
	} {
		p := filepath.Join(dir, "t.json")
		if err := os.WriteFile(p, []byte(`{"`+key+`":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := Load(p)
		if err == nil {
			t.Errorf("%s: loaded", name)
			continue
		}
		if msg := err.Error(); len(msg) > 400 || strings.Contains(msg, strings.Repeat("k", 100)) || strings.Contains(msg, "\x1b") {
			t.Errorf("%s: error echoes the file (%d bytes): %.200q", name, len(msg), msg)
		}
	}
}

// Every theme error that repeats text from the file is bounded: a 5,000-character name, base, colour,
// font or key (in a type error too) gives a message of a few hundred characters at most, and an
// escape sequence in it is written out, not sent.
func TestLoad_LongValuesGiveBoundedErrors(t *testing.T) {
	long := strings.Repeat("k", 5000)
	dir := t.TempDir()
	for name, doc := range map[string]string{
		"name":       `{"bide_theme":1,"name":"` + long + `","base":"` + baseName(t) + `"}`,
		"base":       `{"bide_theme":1,"name":"mine","base":"` + long + `"}`,
		"colour":     `{"bide_theme":1,"name":"mine","base":"` + baseName(t) + `","surface":{"bg":"` + long + `"}}`,
		"font":       `{"bide_theme":1,"name":"mine","base":"` + baseName(t) + `","font_ui":"` + long + `"}`,
		"unknown":    `{"bide_theme":1,"name":"mine","base":"` + baseName(t) + `","surface":{"` + long + `":"#fff"}}`,
		"type error": `{"bide_theme":1,"name":"mine","base":"` + baseName(t) + `","surface":{"` + long + `":5}}`,
		"top key":    `{"bide_theme":1,"name":"mine","base":"` + baseName(t) + `","` + long + `":5}`,
	} {
		p := filepath.Join(dir, "t.json")
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := Load(p)
		if err == nil {
			t.Errorf("%s: loaded", name)
			continue
		}
		if msg := err.Error(); len(msg) > 800 || strings.Contains(msg, strings.Repeat("k", 300)) {
			t.Errorf("%s: error is %d bytes and repeats the value: %.300q", name, len(msg), msg)
		}
	}
}

func baseName(t *testing.T) string { return "light" }
