package flowedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

func isComposite(v any) bool {
	switch v.(type) {
	case []any, Map:
		return true
	}
	return false
}

// scalar renders a scalar. q is the quote of the scalar it replaces (0 plain).
func scalar(v any, q byte, flow, js bool) (string, error) {
	switch x := v.(type) {
	case int:
		return strconv.Itoa(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case string:
		if !utf8.ValidString(x) {
			return "", refuse("a string that is not valid UTF-8")
		}
		if strings.ContainsAny(x, "\n\r\t") {
			return "", refuse("a string with a line break or tab")
		}
		if js {
			var b bytes.Buffer
			e := json.NewEncoder(&b)
			e.SetEscapeHTML(false)
			if err := e.Encode(x); err != nil {
				return "", err
			}
			out := strings.TrimSuffix(b.String(), "\n")
			for _, r := range []rune{'\u2028', '\u2029', '\u0085', '\ufeff'} { // raw, they are line breaks or a mark to some readers
				out = strings.ReplaceAll(out, string(r), fmt.Sprintf(`\u%04x`, r))
			}
			return out, nil
		}
		dq := func() string {
			b, _ := yaml.Marshal(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x, Style: yaml.DoubleQuotedStyle})
			return strings.TrimSuffix(string(b), "\n")
		}
		// A rune a YAML 1.1 reader reads as a line break, or that is a control or format character,
		// is written escaped in double quotes, never raw in a single-quoted or plain scalar.
		if strings.ContainsFunc(x, func(r rune) bool {
			return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || r == '\ufeff'
		}) {
			return dq(), nil
		}
		switch {
		case q == '"':
			return dq(), nil
		case q == '\'':
			return "'" + strings.ReplaceAll(x, "'", "''") + "'", nil
		}
		b, err := yaml.Marshal(x)
		if err != nil {
			return "", err
		}
		out := strings.TrimSuffix(string(b), "\n")
		if strings.Contains(out, "\n") {
			return "", refuse("a string the library folds")
		}
		if flow && !strings.HasPrefix(out, "\"") && !strings.HasPrefix(out, "'") && strings.ContainsAny(out, ",[]{}") {
			return dq(), nil
		}
		return out, nil
	}
	return "", refuse("value type %T", v)
}

func flowValue(v any, js bool) (string, error) {
	switch x := v.(type) {
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			s, err := flowValue(e, js)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case Map:
		parts := make([]string, len(x))
		for i, kv := range x {
			k, err := scalar(kv.Key, 0, true, js)
			if err != nil {
				return "", err
			}
			s, err := flowValue(kv.Val, js)
			if err != nil {
				return "", err
			}
			parts[i] = k + ": " + s
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	}
	return scalar(v, 0, true, js)
}

func (d *doc) flowItem(isMap bool, op Op, _ bool) (string, error) {
	v, err := flowValue(op.Value, d.js)
	if err != nil {
		return "", err
	}
	if !isMap {
		return v, nil
	}
	k, err := scalar(op.Key, 0, true, d.js)
	if err != nil {
		return "", err
	}
	return k + ": " + v, nil
}

func (d *doc) blockEntry(pad, key string, v any, flow bool) (string, error) {
	k, err := scalar(key, 0, false, false)
	if err != nil {
		return "", err
	}
	if !isComposite(v) {
		s, err := scalar(v, 0, false, false)
		return pad + k + ": " + s + "\n", err
	}
	if flow || empty(v) {
		s, err := flowValue(v, false)
		return pad + k + ": " + s + "\n", err
	}
	inner := pad + strings.Repeat(" ", d.indent)
	switch x := v.(type) {
	case Map:
		body, err := d.blockMap(inner, x)
		return pad + k + ":\n" + body, err
	case []any:
		var b strings.Builder
		for _, e := range x {
			s, err := d.blockItem(inner, 2, e, false)
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		}
		return pad + k + ":\n" + b.String(), nil
	}
	return "", refuse("value")
}

func empty(v any) bool {
	switch x := v.(type) {
	case []any:
		return len(x) == 0
	case Map:
		return len(x) == 0
	}
	return false
}

func (d *doc) blockMap(pad string, m Map) (string, error) {
	var b strings.Builder
	for _, kv := range m {
		s, err := d.blockEntry(pad, kv.Key, kv.Val, false)
		if err != nil {
			return "", err
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// blockItem renders a sequence item whose dash is at pad and whose content starts gap columns on.
func (d *doc) blockItem(pad string, gap int, v any, flow bool) (string, error) {
	dash := pad + "-" + strings.Repeat(" ", gap-1)
	if !isComposite(v) {
		s, err := scalar(v, 0, false, false)
		return dash + s + "\n", err
	}
	if flow || empty(v) {
		s, err := flowValue(v, false)
		return dash + s + "\n", err
	}
	m, ok := v.(Map)
	if !ok {
		return "", refuse("a sequence inside a sequence")
	}
	body, err := d.blockMap(pad+strings.Repeat(" ", gap), m)
	if err != nil {
		return "", err
	}
	return dash + strings.TrimPrefix(body, pad+strings.Repeat(" ", gap)), nil
}

// ---- the model: the operation applied to the data ----

func decode(b []byte) (any, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func plain(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	case Map:
		out := make(map[string]any, len(x))
		for _, kv := range x {
			out[kv.Key] = plain(kv.Val)
		}
		return out
	}
	return v
}

func clone(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clone(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = clone(e)
		}
		return out
	}
	return v
}

func model(src []byte, op Op) (any, error) {
	v, err := decode(src)
	if err != nil {
		return nil, refuse("does not parse: %v", err)
	}
	root := clone(v)
	if len(op.Path) == 0 {
		if op.Kind != Insert {
			return nil, refuse("operation on the document")
		}
		m, ok := root.(map[string]any)
		if !ok {
			return nil, refuse("document is not a mapping")
		}
		if _, dup := m[op.Key]; dup {
			return nil, refuse("key exists")
		}
		m[op.Key] = plain(op.Value)
		return root, nil
	}
	return edit(root, op.Path, op), nil
}

// edit returns v with op applied at path (v is owned by the caller).
func edit(v any, path []any, op Op) any {
	if len(path) == 0 {
		switch op.Kind {
		case Set:
			return plain(op.Value)
		case Insert:
			switch x := v.(type) {
			case map[string]any:
				x[op.Key] = plain(op.Value)
				return x
			case []any:
				i := min(max(op.Index, 0), len(x))
				return slices.Insert(x, i, plain(op.Value))
			}
		}
		return v
	}
	last := len(path) == 1
	switch p := path[0].(type) {
	case string:
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		if last && op.Kind == Delete {
			delete(m, p)
			return m
		}
		if last && op.Kind == Set {
			m[p] = plain(op.Value)
			return m
		}
		m[p] = edit(m[p], path[1:], op)
		return m
	case int:
		s, ok := v.([]any)
		if !ok || p >= len(s) {
			return v
		}
		if last && op.Kind == Delete {
			out := slices.Delete(s, p, p+1)
			if out == nil {
				out = []any{}
			}
			return out
		}
		s[p] = edit(s[p], path[1:], op)
		return s
	}
	return v
}

var _ = bytes.Equal
var _ = reflect.DeepEqual
var _ = fmt.Sprint
