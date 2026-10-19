package plan

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// nameCfg is a config with the identifier under test in one field.
func nameCfg(field, v string) string {
	q := func(s string) string { return fmt.Sprintf("%q", s) }
	flow, entry, node, block := `"f"`, `"a"`, `"a"`, `"b"`
	edge, sw, pred, to, els, join, in, merge := `["a","b"]`, `"b"`, `"p"`, `"a"`, `"a"`, `"j"`, `["a","b"]`, `"m"`
	switch field {
	case "flow":
		flow = q(v)
	case "entry":
		entry = q(v)
	case "nodes[0].name":
		node = q(v)
	case "nodes[0].block":
		block = q(v)
	case "wiring[0].edge[1]":
		edge = `["a",` + q(v) + `]`
	case "wiring[1].switch":
		sw = q(v)
	case "wiring[1].when[0].pred":
		pred = q(v)
	case "wiring[1].when[0].to":
		to = q(v)
	case "wiring[1].else":
		els = q(v)
	case "wiring[2].join":
		join = q(v)
	case "wiring[2].inputs[1]":
		in = `["a",` + q(v) + `]`
	case "wiring[2].merge":
		merge = q(v)
	}
	return `{"version":1,"flow":` + flow + `,"entry":` + entry + `,"nodes":[{"name":` + node + `,"block":` + block + `},{"name":"b","block":"b"}],` +
		`"wiring":[{"edge":` + edge + `},{"switch":` + sw + `,"when":[{"pred":` + pred + `,"to":` + to + `}],"else":` + els + `},{"join":` + join + `,"inputs":` + in + `,"merge":` + merge + `}]}`
}

var nonPrinting = map[string]string{
	"right-to-left override": "ch\u202egra",
	"zero-width space":       "\u200b",
	"zero-width joiner":      "a\u200db",
	"soft hyphen":            "a\u00adb",
	"no-break space":         "a\u00a0b",
	"line separator":         "a\u2028b",
	"byte order mark":        "a\ufeffb",
	"private use":            "a\ue000b",
	"tab":                    "a\tb",
	"leading space":          " a",
	"trailing space":         "a ",
}

var identifierFields = []string{"flow", "entry", "nodes[0].name", "nodes[0].block", "wiring[0].edge[1]", "wiring[1].switch", "wiring[1].when[0].pred", "wiring[1].when[0].to", "wiring[1].else", "wiring[2].join", "wiring[2].inputs[1]", "wiring[2].merge"}

// A config whose identifier holds a character that does not print is refused, by Load and by the check
// every tool shares, and the error names the field and writes the character out.
func TestConfigRefusesNonPrintingIdentifiers(t *testing.T) {
	for _, field := range identifierFields {
		for what, v := range nonPrinting {
			data := []byte(nameCfg(field, v))
			var cfg config
			err := parseConfig(data, &cfg)
			if err == nil || !errors.Is(err, agent.ErrConfig) {
				t.Errorf("%s with %s: parse gave %v", field, what, err)
				continue
			}
			if !strings.Contains(err.Error(), field+" ") {
				t.Errorf("%s with %s: the error does not name the field: %v", field, what, err)
			}
			if strings.ContainsFunc(err.Error(), func(r rune) bool { return r > 0x7e || r == '\t' }) {
				t.Errorf("%s with %s: the error holds a raw character: %q", field, what, err)
			}
			rep := InspectConfig(data, nil)
			if !rep.Errors() {
				t.Errorf("%s with %s: the check reports no error", field, what)
			}
			if _, err := Load[struct{}, struct{}](data, NewRegistry()); err == nil {
				t.Errorf("%s with %s: Load accepted it", field, what)
			}
		}
	}
}

func TestNonPrintingErrorShowsTheCharacter(t *testing.T) {
	var cfg config
	err := parseConfig([]byte(nameCfg("nodes[0].name", "ch\u202egra")), &cfg)
	if err == nil || !strings.Contains(err.Error(), `nodes[0].name "ch\u{202E}gra"`) || !strings.Contains(err.Error(), "U+202E") {
		t.Fatalf("%v", err)
	}
}

// Names people use are untouched: spaces inside, other scripts, accents, emoji, punctuation, and an
// assert line with padding.
func TestConfigAcceptsOrdinaryNames(t *testing.T) {
	for _, v := range []string{"manual review", "é", "ノード", "\U0001F680 rocket", "a-b_c.d", "x:y", "1e3"} {
		for _, field := range identifierFields {
			var cfg config
			if err := parseConfig([]byte(nameCfg(field, v)), &cfg); err != nil {
				t.Errorf("%s = %q: %v", field, v, err)
			}
		}
	}
	var cfg config
	data := `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"}],"wiring":[],"assert":["  a reachable  "]}`
	if err := parseConfig([]byte(data), &cfg); err != nil {
		t.Errorf("a padded assert line: %v", err)
	}
	// An assert line with an invisible character is refused.
	data = `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"}],"wiring":[],"assert":["a\u202e reachable"]}`
	if err := parseConfig([]byte(data), &cfg); err == nil || !strings.Contains(err.Error(), "assert[0]") {
		t.Errorf("an assert line with an override: %v", err)
	}
}

// An approver id with such a character is refused by the gate's own rule, as it always was.
func TestApproverIDsWithNonPrintingCharactersStayRefused(t *testing.T) {
	data := `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","approval":{"need":1,"approvers":["ops\u202e"]}}],"wiring":[]}`
	rep := InspectConfig([]byte(data), nil)
	if !rep.Errors() {
		t.Error("an approver id with a direction override was accepted")
	}
}

// Blank-rendering characters are refused like invisible ones: a name of one filler reads as empty.
func TestConfigRefusesBlankRenderingNames(t *testing.T) {
	for what, v := range map[string]string{
		"hangul filler": "\u3164", "halfwidth hangul filler": "ab\uffa0", "braille blank": "a\u2800", "grapheme joiner": "a\u034f",
		"khmer inherent vowel": "a\u17b4", "bare variation selector": "\ufe0f", "leading combining mark": "\u0301a",
	} {
		var cfg config
		if err := parseConfig([]byte(nameCfg("nodes[0].name", v)), &cfg); err == nil {
			t.Errorf("%s: %q was accepted", what, v)
		}
	}
	for _, v := range []string{"e\u0301", "\u2764\ufe0f", "\u05e9\u05dc\u05d5\u05dd", "\u0645\u0631\u062d\u0628\u0627", "\ud55c\uae00"} {
		var cfg config
		if err := parseConfig([]byte(nameCfg("nodes[0].name", v)), &cfg); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// An assert line may separate its words with any whitespace the grammar collapses (a tab, for one).
func TestAssertLineWhitespaceIsAccepted(t *testing.T) {
	var cfg config
	data := `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"}],"wiring":[],"assert":["a\treachable","  a   reachable  "]}`
	if err := parseConfig([]byte(data), &cfg); err != nil {
		t.Errorf("%v", err)
	}
}
