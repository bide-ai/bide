package amount

// The cap-rule grammar (note section 6.2). This file is the whole of it: the types a rule parses
// to, the parser, and the grammar in the comment below. A change of syntax is a change to this
// file and to the examples TestDocGrammar parses in the documents; nothing else reads the text.
//
// A rule is one line:
//
//	rule    = name ":" target ( "> " amount | "sum " amount " per " window [ " by " key ] ) "->" action
//	target  = tool { "|" tool } "." member { "." member } "[" unit { "," option } "]"
//	unit    = "minor" | "major"
//	option  = "currency=" member { "." member } | "exponent=" digits
//	amount  = digits [ "." digits ] " " currency        (in major units, whatever the unit)
//	window  = "run" | "hour" | "day"
//	key     = "principal" | member { "." member } { "lower" | "trim" }
//	action  = "gate" [ "(" gatename ")" ] | "refuse"
//
// name, tool, member and gatename are ASCII letters, digits, '_' and '-' (a name starts with a
// letter and is at most 64 bytes). Spaces and tabs separate tokens and may be dropped where
// nothing runs together. The line is printable ASCII, one line, at most 1024 bytes.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrRule is wrapped by every error ParseRule and ParseRules return. The engine wraps it in
// agent.ErrConfig at registration.
var ErrRule = errors.New("cap rule")

// Kind is the form of a rule.
type Kind uint8

const (
	// PerCall: "> N CUR", a limit on one call's amount.
	PerCall Kind = iota + 1
	// Sum: "sum N CUR per window", a limit on the total over a window.
	Sum
)

// Window is the period a cumulative rule sums over. Hour and Day are tumbling windows in UTC
// (decision 11).
type Window uint8

const (
	Run Window = iota + 1
	Hour
	Day
)

func (w Window) String() string {
	switch w {
	case Run:
		return "run"
	case Hour:
		return "hour"
	case Day:
		return "day"
	}
	return "window(?)"
}

// ActionKind is what a rule does to a call over its limit.
type ActionKind uint8

const (
	// Gate holds the call for approval.
	Gate ActionKind = iota + 1
	// Refuse does not run the call and does not ask.
	Refuse
)

// Action is the rule's action; GateName names the gate a host registered, empty for the
// escalation gate.
type Action struct {
	Kind     ActionKind
	GateName string
}

// Key is what a cumulative rule is keyed by: the run's principal, or a member of the arguments,
// normalised as stated. The engine canonicalises further (a digest of the normalised text).
type Key struct {
	Principal bool
	Path      []string
	Lower     bool // strings.ToLower the member's text
	Trim      bool // strings.TrimSpace the member's text
}

// Rule is one parsed cap line. Registration-time checks against a tool's input schema are the
// engine's; the parser checks only the line.
type Rule struct {
	Name        string
	Tools       []string // the set of tools sharing this rule's ledger
	Arg         []string // the member path of the amount in each tool's arguments
	Unit        Unit
	CurrencyArg []string // the member holding the call's currency; nil for a fixed currency
	Currency    string   // the currency the limit is in
	Exponent    int      // the currency's exponent, from the table or exponent=
	Limit       int64    // in minor units
	Kind        Kind
	Window      Window // Sum only
	Key         *Key   // Sum only; nil for an unkeyed total
	Action      Action
}

const (
	maxLine = 1024
	maxName = 64
)

// ParseRules parses lines, one rule each, and refuses a name used twice. A name is the ledger's
// key (6.5), so a name differing from another only in case is refused too: it is almost surely a
// mistake, and the two would be different ledgers.
func ParseRules(lines ...string) ([]Rule, error) {
	var rules []Rule
	seen := map[string]int{}
	for i, line := range lines {
		r, err := ParseRule(line)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
		key := strings.ToLower(r.Name)
		if j, dup := seen[key]; dup {
			return nil, fmt.Errorf("rule %d: %w: name %q repeats rule %d's (names are compared without case)", i+1, ErrRule, r.Name, j+1)
		}
		seen[key] = i
		rules = append(rules, r)
	}
	return rules, nil
}

// ParseRule parses one rule line. The error wraps ErrRule and names the column and what was
// expected; it does not echo the line.
func ParseRule(line string) (Rule, error) {
	if len(line) > maxLine {
		return Rule{}, fmt.Errorf("%w: line too long (over %d bytes)", ErrRule, maxLine)
	}
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\n' || c == '\r':
			return Rule{}, fmt.Errorf("%w: a rule must be a single line", ErrRule)
		case c != '\t' && (c < 0x20 || c > 0x7e):
			return Rule{}, fmt.Errorf("%w: a rule is printable ASCII (column %d)", ErrRule, i+1)
		}
	}
	if strings.TrimSpace(line) == "" {
		return Rule{}, fmt.Errorf("%w: rule is empty", ErrRule)
	}
	p := &parser{s: line}
	r, err := p.rule()
	if err != nil {
		return Rule{}, err
	}
	return r, nil
}

type parser struct {
	s string
	i int
}

func (p *parser) fail(format string, args ...any) error {
	return fmt.Errorf("%w: column %d: %s", ErrRule, p.i+1, fmt.Sprintf(format, args...))
}

func (p *parser) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *parser) done() bool { return p.i >= len(p.s) }

func (p *parser) peek() byte {
	if p.done() {
		return 0
	}
	return p.s[p.i]
}

func (p *parser) accept(c byte) bool {
	if p.peek() == c && !p.done() {
		p.i++
		return true
	}
	return false
}

func (p *parser) acceptStr(s string) bool {
	if strings.HasPrefix(p.s[p.i:], s) {
		p.i += len(s)
		return true
	}
	return false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }

// ident scans [A-Za-z0-9_-]*; a '-' that begins "->" ends it.
func (p *parser) ident() string {
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if isLetter(c) || isDigit(c) || c == '_' || (c == '-' && !strings.HasPrefix(p.s[p.i:], "->")) {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i]
}

// word scans letters: a keyword, or a word that is not one.
func (p *parser) word() string {
	start := p.i
	for p.i < len(p.s) && isLetter(p.s[p.i]) {
		p.i++
	}
	return p.s[start:p.i]
}

// path scans member { "." member }, stopping before anything else. what names it for errors.
func (p *parser) path(what string) ([]string, error) {
	var out []string
	for {
		m := p.ident()
		if m == "" {
			return nil, p.fail("expected %s", what)
		}
		out = append(out, m)
		if !p.accept('.') {
			return out, nil
		}
	}
}

func (p *parser) rule() (Rule, error) {
	var r Rule
	p.ws()
	if !isLetter(p.peek()) {
		return r, p.fail("expected the rule name (a letter, then letters, digits, '_' or '-')")
	}
	r.Name = p.ident()
	if len(r.Name) > maxName {
		return r, p.fail("rule name is longer than %d bytes", maxName)
	}
	p.ws()
	if !p.accept(':') {
		return r, p.fail("expected ':' after the rule name")
	}
	p.ws()
	override, err := p.target(&r)
	if err != nil {
		return r, err
	}
	p.ws()
	switch {
	case p.accept('>'):
		r.Kind = PerCall
	case p.acceptStr("sum") && (p.peek() == ' ' || p.peek() == '\t'):
		r.Kind = Sum
	default:
		return r, p.fail("expected '>' or 'sum' after the target")
	}
	p.ws()
	if err := p.limit(&r, override); err != nil {
		return r, err
	}
	if r.Kind == Sum {
		if err := p.window(&r); err != nil {
			return r, err
		}
	}
	p.ws()
	if !p.acceptStr("->") {
		return r, p.fail("expected '->' and an action")
	}
	p.ws()
	if err := p.action(&r); err != nil {
		return r, err
	}
	p.ws()
	if !p.done() {
		return r, p.fail("trailing text after the rule")
	}
	return r, nil
}

// target parses tool { "|" tool } "." path "[" unit { "," option } "]" and returns the exponent=
// override, if any, for limit to resolve once it has read the currency.
func (p *parser) target(r *Rule) (*int, error) {
	for {
		t := p.ident()
		if t == "" {
			return nil, p.fail("expected a tool name")
		}
		for _, have := range r.Tools {
			if have == t {
				return nil, p.fail("tool %.40q is listed twice", t)
			}
		}
		r.Tools = append(r.Tools, t)
		if p.accept('|') {
			continue
		}
		if !p.accept('.') {
			return nil, p.fail("expected '|' or '.' after a tool name, then the argument's member name")
		}
		break
	}
	var err error
	if r.Arg, err = p.path("the argument's member name"); err != nil {
		return nil, err
	}
	if !p.accept('[') {
		return nil, p.fail("expected '[' and a unit after the argument")
	}
	p.ws()
	switch u := p.word(); u {
	case "minor":
		r.Unit = Minor
	case "major":
		r.Unit = Major
	default:
		return nil, p.fail("expected the unit minor or major")
	}
	var override *int
	haveCurrency := false
	for {
		p.ws()
		if p.accept(']') {
			return override, nil
		}
		if !p.accept(',') {
			return nil, p.fail("expected ',' or ']'")
		}
		p.ws()
		switch name := p.word(); name {
		case "currency":
			if haveCurrency {
				return nil, p.fail("currency= given twice")
			}
			if !p.accept('=') {
				return nil, p.fail("expected '=' after currency")
			}
			if r.CurrencyArg, err = p.path("a member path after currency="); err != nil {
				return nil, err
			}
			haveCurrency = true
		case "exponent":
			if override != nil {
				return nil, p.fail("exponent= given twice")
			}
			if !p.accept('=') {
				return nil, p.fail("expected '=' after exponent")
			}
			d := p.s[p.i : p.i+digitsAt(p.s[p.i:])]
			n, perr := strconv.Atoi(d)
			if d == "" || (len(d) > 1 && d[0] == '0') || perr != nil || n > MaxExponent {
				return nil, p.fail("exponent must be a whole number from 0 to %d", MaxExponent)
			}
			p.i += len(d)
			override = &n
		case "":
			return nil, p.fail("expected an option (currency= or exponent=) after ','")
		default:
			return nil, p.fail("unknown option %.20q (want currency= or exponent=)", name)
		}
	}
}

// limit parses "N CUR" and sets Currency, Exponent and Limit. The limit is in major units
// whatever the argument's unit, with at most the currency's exponent of fraction digits.
func (p *parser) limit(r *Rule, override *int) error {
	n := digitsAt(p.s[p.i:])
	if n > 0 && p.s[p.i+n:] != "" && p.s[p.i+n] == '.' {
		if m := digitsAt(p.s[p.i+n+1:]); m > 0 {
			n += 1 + m
		}
	}
	if n == 0 {
		return p.fail("expected an amount (digits, optionally a point and digits)")
	}
	text := p.s[p.i : p.i+n]
	p.i += n
	p.ws()
	if p.i+3 > len(p.s) || !isCode(p.s[p.i:p.i+3]) || (p.i+3 < len(p.s) && (isLetter(p.s[p.i+3]) || isDigit(p.s[p.i+3]))) {
		return p.fail("expected a currency code of three upper-case letters after the amount")
	}
	r.Currency = p.s[p.i : p.i+3]
	p.i += 3
	exp, err := Resolve(r.Currency, override)
	if err != nil {
		return fmt.Errorf("%w: %v (state exponent=N for a code outside the table)", ErrRule, err)
	}
	r.Exponent = exp
	v, err := Parse(text, Major, exp)
	if err != nil {
		return fmt.Errorf("%w: limit: %v", ErrRule, err)
	}
	r.Limit = v
	return nil
}

// window parses "per window [ by key ]".
func (p *parser) window(r *Rule) error {
	p.ws()
	if !p.acceptStr("per") || p.peek() != ' ' && p.peek() != '\t' {
		return p.fail("expected 'per' and a window (run, hour or day)")
	}
	p.ws()
	switch w := p.ident(); w {
	case "run":
		r.Window = Run
	case "hour":
		r.Window = Hour
	case "day":
		r.Window = Day
	default:
		return p.fail("unknown window %.20q (want run, hour or day)", w)
	}
	p.ws()
	save := p.i
	if !p.acceptStr("by") || p.peek() != ' ' && p.peek() != '\t' {
		p.i = save
		return nil
	}
	p.ws()
	path, err := p.path("a key: principal, or an argument member path")
	if err != nil {
		return err
	}
	r.Key = &Key{Path: path}
	if len(path) == 1 && path[0] == "principal" {
		r.Key = &Key{Principal: true}
	}
	for {
		p.ws()
		if strings.HasPrefix(p.s[p.i:], "->") {
			return nil
		}
		w := p.word()
		switch {
		case w == "lower" || w == "trim":
			if r.Key.Principal {
				return p.fail("the principal key takes no normalisation")
			}
			flag := &r.Key.Lower
			if w == "trim" {
				flag = &r.Key.Trim
			}
			if *flag {
				return p.fail("%s given twice", w)
			}
			*flag = true
		default:
			return p.fail("unexpected %.20q: expected lower, trim or '->'", w)
		}
	}
}

// action parses gate, gate(name) or refuse.
func (p *parser) action(r *Rule) error {
	switch a := p.word(); a {
	case "gate":
		r.Action.Kind = Gate
		if p.accept('(') {
			name := p.ident()
			if name == "" {
				return p.fail("expected a gate name")
			}
			if !p.accept(')') {
				return p.fail("expected ')' after the gate name")
			}
			r.Action.GateName = name
		}
	case "refuse":
		r.Action.Kind = Refuse
		if p.peek() == '(' {
			return p.fail("refuse takes no gate name")
		}
	case "":
		return p.fail("expected an action: gate or refuse")
	default:
		return p.fail("unknown action %.20q (want gate or refuse)", a)
	}
	return nil
}
