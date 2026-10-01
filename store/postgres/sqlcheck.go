package postgres

import (
	"errors"
	"fmt"
	"strings"
)

// The statement check. Every statement the store sends on the pool is tokenized and held to
// allowlists. Its rule is that it refuses anything whose resolution it cannot vouch for: every
// name Postgres would look up through the search path must be qualified, or refused. A statement
// may:
//
//   - call only a function in sqlFunctions, each qualified with pg_catalog, or the next_seq
//     function under its exact schema-qualified name, once, in an INSERT with no SELECT. A name
//     followed by "(" is a call unless it is a reserved keyword that takes a list and cannot name
//     a function (sqlListWords), CONFLICT after ON, or the table (or its alias) after INTO.
//   - use an operator only written OPERATOR(pg_catalog.<op>), with <op> in sqlOperators: a bare
//     operator resolves through the search path, and pg_catalog has no exact match for some
//     argument types (text[] = text[], oid = regtype), so an operator a role creates with those
//     types anywhere on the path would win. The one bare operator is the "=" of an assignment in
//     a SET list, which is syntax, not an operator.
//   - use none of the keywords that resolve an operator or a function implicitly (sqlRefused):
//     LIKE, ILIKE, SIMILAR, IS [NOT] DISTINCT, OVERLAPS, BETWEEN, IN, CASE, USING, and ARRAY
//     constructors.
//   - name as a relation (after FROM, JOIN, INTO or UPDATE, or in a FROM list) only the store's own tables
//     under their exact schema-qualified names, a pg_catalog relation, a subquery, or a call of
//     an allowed function.
//   - cast ("::") only to a type in sqlTypes, qualified with pg_catalog: a domain's CHECK can
//     call any function, and an unqualified type resolves through the search path.
//   - use only ASCII outside string literals and quoted identifiers, so no name ends where a
//     scan of the text would not expect it.
//
// The tokenizer refuses what it cannot follow: a semicolon (a second statement under the simple
// protocol), a comment, a dollar-quoted string, a backslash (an escape in a string literal when
// standard_conforming_strings is off), a typed literal (a name before a string literal, which also
// covers E'...', U&'...' and B'...'), and any byte outside the tokens below. A Unicode-escaped
// identifier, U&"...", is an identifier followed by the operator "&", which is refused.
//
// What Postgres still resolves without a name in the statement does not go through the search
// path: ORDER BY, DISTINCT and the ON CONFLICT arbiter use the type's default operator class and
// the table's indexes, and casts use pg_cast.

// sqlTok is one token of a statement: an identifier ('i', lower-cased), a quoted identifier ('q',
// its name), a string literal ('s'), a parameter ('$'), a number ('n'), an operator ('o'), or
// punctuation ('p': one of ( ) , . [ ] and ::).
type sqlTok struct {
	kind byte
	text string
}

var (
	// sqlFunctions are the functions a statement may call.
	sqlFunctions = map[string]bool{
		"pg_catalog.now": true, "pg_catalog.max": true, "pg_catalog.starts_with": true,
		// the catalog reads in checkSchema and storeSchema
		"pg_catalog.array_agg": true, "pg_catalog.unnest": true, "pg_catalog.current_schema": true,
		"pg_catalog.current_schemas": true, "pg_catalog.array_position": true,
	}
	// sqlListWords are the keywords the store's statements follow with a parenthesized list or
	// subquery. None can name a function: VALUES, ANY, FROM, WHERE, SELECT, ON, AND, OR and NOT
	// are reserved, and EXISTS and COALESCE are keywords that cannot be function names.
	sqlListWords = map[string]bool{"values": true, "exists": true, "any": true, "coalesce": true,
		"from": true, "where": true, "select": true, "on": true, "and": true, "or": true, "not": true}
	// sqlRefused are the keywords that resolve an operator or a function through the search path
	// without naming it (ARRAY builds an array the check cannot type).
	sqlRefused = map[string]bool{"like": true, "ilike": true, "similar": true, "overlaps": true, "between": true,
		"in": true, "case": true, "using": true, "array": true}
	// sqlTypes are the types a statement may cast to, each written pg_catalog.<type>, optionally
	// as an array (<type>[]).
	sqlTypes = map[string]bool{"text": true, "int8": true, "int2": true, "float8": true, "interval": true, "oidvector": true, "regtype": true, "oid": true}
	// sqlOperators are the operators a statement may use.
	sqlOperators = map[string]bool{"=": true, "<>": true, "<": true, ">": true, "<=": true, ">=": true, "+": true, "-": true, "*": true}
)

const sqlOperatorChars = "+-*/<>=~!@#%^&|`?"

// sqlOwner names what the check guards, for its errors.
const sqlOwner = "store"

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdentByte(c byte) bool  { return isIdentStart(c) || isDigit(c) }

// sqlTokens splits q into tokens, or returns an error for anything the check cannot follow.
func sqlTokens(q string) ([]sqlTok, error) {
	var toks []sqlTok
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'':
			j := i + 1
			for {
				if j >= len(q) {
					return nil, errors.New("an unterminated string literal")
				}
				if q[j] == '\\' {
					return nil, errors.New("a backslash in a string literal")
				}
				if q[j] == '\'' {
					if j+1 < len(q) && q[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			toks = append(toks, sqlTok{'s', q[i : j+1]})
			i = j + 1
		case c == '"':
			var name strings.Builder
			j := i + 1
			for {
				if j >= len(q) {
					return nil, errors.New("an unterminated quoted identifier")
				}
				if q[j] == '"' {
					if j+1 < len(q) && q[j+1] == '"' {
						name.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				name.WriteByte(q[j])
				j++
			}
			if name.Len() == 0 {
				return nil, errors.New("an empty quoted identifier")
			}
			toks = append(toks, sqlTok{'q', name.String()})
			i = j + 1
		case c == '$':
			j := i + 1
			for j < len(q) && isDigit(q[j]) {
				j++
			}
			if j == i+1 {
				return nil, errors.New("a dollar-quoted string")
			}
			toks = append(toks, sqlTok{'$', q[i:j]})
			i = j
		case isDigit(c):
			j := i
			for j < len(q) && isDigit(q[j]) {
				j++
			}
			if j < len(q) && (isIdentByte(q[j]) || q[j] == '.') {
				return nil, fmt.Errorf("a number the check does not read, at %q", q[i:min(j+1, len(q))])
			}
			toks = append(toks, sqlTok{'n', q[i:j]})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(q) && isIdentByte(q[j]) {
				j++
			}
			if j < len(q) && q[j] == '$' { // a non-ASCII byte is refused as the next token
				return nil, fmt.Errorf("an identifier with a byte the check does not read, at %q", q[i:min(j+1, len(q))])
			}
			toks = append(toks, sqlTok{'i', strings.ToLower(q[i:j])})
			i = j
		case strings.IndexByte(sqlOperatorChars, c) >= 0:
			j := i
			for j < len(q) && strings.IndexByte(sqlOperatorChars, q[j]) >= 0 {
				j++
			}
			if op := q[i:j]; !sqlOperators[op] {
				return nil, fmt.Errorf("the operator or comment %q", op)
			}
			toks = append(toks, sqlTok{'o', q[i:j]})
			i = j
		case c == ':':
			if i+1 >= len(q) || q[i+1] != ':' {
				return nil, errors.New("a colon that is not a cast")
			}
			toks = append(toks, sqlTok{'p', "::"})
			i += 2
		case strings.IndexByte("(),.[]", c) >= 0:
			toks = append(toks, sqlTok{'p', string(c)})
			i++
		default:
			return nil, fmt.Errorf("the byte %q", c)
		}
	}
	return toks, nil
}

// isTok reports whether toks[i] exists and is of kind with text.
func isTok(toks []sqlTok, i int, kind byte, text string) bool {
	return i >= 0 && i < len(toks) && toks[i].kind == kind && toks[i].text == text
}

// sqlName returns the dotted name starting at toks[i] (identifiers and quoted identifiers joined by
// "."), as its parts, and the index of its last token.
func sqlName(toks []sqlTok, i int) ([]sqlTok, int) {
	name := []sqlTok{toks[i]}
	for i+2 < len(toks) && isTok(toks, i+1, 'p', ".") && (toks[i+2].kind == 'i' || toks[i+2].kind == 'q') {
		name = append(name, toks[i+2])
		i += 2
	}
	return name, i
}

// qualifiedBuiltin returns "pg_catalog.<name>" for a name written as two unquoted identifiers
// pg_catalog.<name>, or "".
func qualifiedBuiltin(name []sqlTok) string {
	if len(name) == 2 && name[0].kind == 'i' && name[0].text == "pg_catalog" && name[1].kind == 'i' {
		return "pg_catalog." + name[1].text
	}
	return ""
}

// sameName reports whether two dotted names are the same, part for part.
func sameName(a, b []sqlTok) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkSQL returns an error unless q follows the rules above. nextSeq, when not nil, is the
// schema-qualified name of the next_seq function, which q may call once if it is an INSERT with no
// SELECT; rels are the qualified names of the relations, other than pg_catalog's, q may name.
func checkSQL(q string, nextSeq []sqlTok, rels [][]sqlTok) error {
	toks, err := sqlTokens(q)
	if err != nil {
		return err
	}
	if len(toks) == 0 {
		return errors.New("an empty statement")
	}
	if nextSeq != nil {
		if !isTok(toks, 0, 'i', "insert") {
			return errors.New("next_seq outside an INSERT")
		}
		for _, t := range toks {
			if t.kind == 'i' && t.text == "select" {
				return errors.New("next_seq in an INSERT ... SELECT")
			}
		}
	}
	calledNextSeq := false
	inSet := false            // in a SET list, where "column =" is an assignment
	fromList := []bool{false} // per parenthesis depth: in a FROM list, where "," precedes a relation
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		depth := len(fromList) - 1
		switch {
		case t.kind == 'p' && t.text == "(":
			fromList = append(fromList, false)
			continue
		case t.kind == 'p' && t.text == ")":
			if depth > 0 {
				fromList = fromList[:depth]
			}
			continue
		case t.kind == 'p' && t.text == "::":
			typ, end := []sqlTok(nil), i
			if i+1 < len(toks) && (toks[i+1].kind == 'i' || toks[i+1].kind == 'q') {
				typ, end = sqlName(toks, i+1)
			}
			if b := qualifiedBuiltin(typ); b == "" || !sqlTypes[strings.TrimPrefix(b, "pg_catalog.")] {
				return fmt.Errorf("a cast to a type other than a pg_catalog built-in in %v", sqlTypeNames())
			}
			if isTok(toks, end+1, 'p', "[") && isTok(toks, end+2, 'p', "]") {
				end += 2
			}
			i = end
			continue
		case t.kind == 'o':
			if t.text == "=" && inSet && i >= 2 && toks[i-1].kind == 'i' && (isTok(toks, i-2, 'i', "set") || isTok(toks, i-2, 'p', ",")) {
				continue // an assignment
			}
			return fmt.Errorf("the bare operator %q, which resolves through the search path; write OPERATOR(pg_catalog.%s)", t.text, t.text)
		}
		if t.kind != 'i' && t.kind != 'q' {
			continue
		}
		if t.kind == 'i' {
			switch {
			case t.text == "operator" && isTok(toks, i+1, 'p', "("):
				if !isTok(toks, i+2, 'i', "pg_catalog") || !isTok(toks, i+3, 'p', ".") || i+4 >= len(toks) ||
					toks[i+4].kind != 'o' || !sqlOperators[toks[i+4].text] || !isTok(toks, i+5, 'p', ")") {
					return errors.New("an OPERATOR(...) other than OPERATOR(pg_catalog.<op>) with an allowed op")
				}
				i += 5
				continue
			case sqlRefused[t.text]:
				return fmt.Errorf("the keyword %s, which resolves an operator or function through the search path", strings.ToUpper(t.text))
			case t.text == "distinct" && (isTok(toks, i-1, 'i', "is") || isTok(toks, i-1, 'i', "not") && isTok(toks, i-2, 'i', "is")):
				return errors.New("IS [NOT] DISTINCT FROM, which resolves an operator through the search path")
			case t.text == "set":
				inSet = true
			case t.text == "where" || t.text == "returning" || t.text == "from":
				inSet = false
			}
			switch t.text {
			case "from":
				fromList[depth] = true
			case "where", "on", "join", "order", "group", "limit", "returning", "having", "union", "set", "values":
				fromList[depth] = false
			}
		}
		name, end := sqlName(toks, i)
		if end+1 < len(toks) && toks[end+1].kind == 's' {
			return fmt.Errorf("a typed literal after %q", t.text)
		}
		if isTok(toks, i-1, 'i', "collate") {
			if !sameName(name, []sqlTok{{'i', "pg_catalog"}, {'q', "C"}}) {
				return errors.New(`a collation other than pg_catalog."C"`)
			}
		}
		call := isTok(toks, end+1, 'p', "(")
		// A relation: after FROM, JOIN, INTO or UPDATE (not DO UPDATE), or after "," in a FROM list.
		relation := isTok(toks, i-1, 'i', "from") || isTok(toks, i-1, 'i', "join") || isTok(toks, i-1, 'i', "into") ||
			isTok(toks, i-1, 'i', "update") && !isTok(toks, i-2, 'i', "do") ||
			isTok(toks, i-1, 'p', ",") && fromList[depth]
		if relation && !(call && !isTok(toks, i-1, 'i', "into")) {
			if !knownRelation(name, rels) {
				return fmt.Errorf("the relation %s, which is neither the %s's own nor pg_catalog's", sqlNameString(name), sqlOwner)
			}
		}
		if call {
			single := len(name) == 1 && name[0].kind == 'i'
			switch {
			case single && sqlListWords[name[0].text]:
			case single && name[0].text == "conflict" && isTok(toks, i-1, 'i', "on"):
			case isTok(toks, i-1, 'i', "into"): // the table whose column list follows
			case single && isTok(toks, i-1, 'i', "as") && afterInto(toks, i-2): // INSERT INTO t AS alias (columns)
			case nextSeq != nil && sameName(name, nextSeq) && !calledNextSeq:
				calledNextSeq = true
			case sqlFunctions[qualifiedBuiltin(name)]:
			default:
				return fmt.Errorf("a call of %s, a function the %s does not know", sqlNameString(name), sqlOwner)
			}
		}
		i = end
	}
	return nil
}

// knownRelation reports whether name is one of rels or a pg_catalog relation.
func knownRelation(name []sqlTok, rels [][]sqlTok) bool {
	if len(name) == 2 && name[0].kind == 'i' && name[0].text == "pg_catalog" && name[1].kind == 'i' {
		return true
	}
	for _, r := range rels {
		if sameName(name, r) {
			return true
		}
	}
	return false
}

// afterInto reports whether toks[end] ends a dotted name that follows INTO.
func afterInto(toks []sqlTok, end int) bool {
	i := end
	for i >= 0 && (toks[i].kind == 'i' || toks[i].kind == 'q') {
		if i >= 2 && isTok(toks, i-1, 'p', ".") {
			i -= 2
			continue
		}
		break
	}
	return i >= 0 && i <= end && (toks[i].kind == 'i' || toks[i].kind == 'q') && isTok(toks, i-1, 'i', "into")
}

// sqlNameString writes a dotted name back as SQL.
func sqlNameString(name []sqlTok) string {
	parts := make([]string, len(name))
	for i, p := range name {
		parts[i] = p.text
		if p.kind == 'q' {
			parts[i] = quoteIdent(p.text)
		}
	}
	return strings.Join(parts, ".")
}

// sqlTypeNames lists sqlTypes for an error message.
func sqlTypeNames() []string {
	var names []string
	for n := range sqlTypes {
		names = append(names, "pg_catalog."+n)
	}
	return names
}

// parseName tokenizes the dotted name n.
func parseName(n string) ([]sqlTok, error) {
	toks, err := sqlTokens(n)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 || toks[0].kind != 'i' && toks[0].kind != 'q' {
		return nil, fmt.Errorf("%q is not a dotted name", n)
	}
	name, end := sqlName(toks, 0)
	if end != len(toks)-1 {
		return nil, fmt.Errorf("%q is not a dotted name", n)
	}
	return name, nil
}
