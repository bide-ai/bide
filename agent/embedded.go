package agent

import (
	"fmt"
	"reflect"
	"unsafe"
)

var toolType = reflect.TypeFor[Tool]()

// checkEmbedded refuses a tool that hides the approval gate or timeout of a tool it embeds. A
// decorator written as struct{ Tool } (overriding Call to log or meter) forwards Name, Safety and
// the rest, but Tool has no Spec method, so the struct has none either and its spec, read from
// the old method set, has no Approval and no Timeout: the gated tool inside would run ungated. The
// agent cannot know whether a decorator meant to drop them, so it fails closed: when any tool
// embedded in t (an anonymous field, at any depth) has an Approval or a Timeout that t's own spec
// s lacks, the tool is ErrConfig. A decorator keeps them by implementing Spec, or by
// implementing Unwrap() Tool, whose spec New reads through Unwrap.
func checkEmbedded(t Tool, s ToolSpec) error {
	var approval, timeout bool
	seen := map[uintptr]bool{}
	var walk func(v reflect.Value, depth int)
	walk = func(v reflect.Value, depth int) {
		if depth > 16 || approval && timeout {
			return
		}
		for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				return
			}
			if v.Kind() == reflect.Pointer {
				if seen[v.Pointer()] {
					return
				}
				seen[v.Pointer()] = true
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return
		}
		if !v.CanAddr() { // an addressable copy, so unexported fields and pointer methods are reachable
			c := reflect.New(v.Type()).Elem()
			c.Set(v)
			v = c
		}
		for i := range v.NumField() {
			if !v.Type().Field(i).Anonymous {
				continue
			}
			f := v.Field(i)
			if !f.CanInterface() {
				f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
			}
			if inner, ok := embeddedTool(f); ok {
				is := specOf(inner)
				approval = approval || is.Approval != nil
				timeout = timeout || is.Timeout > 0
			}
			walk(f, depth+1)
		}
	}
	walk(reflect.ValueOf(t), 0)
	switch {
	case approval && s.Approval == nil:
		return fmt.Errorf("agent: tool %q embeds a tool with an approval gate, and its own spec has none: the decorator hides the approval gate; implement Spec or Unwrap: %w", s.Name, ErrConfig)
	case timeout && s.Timeout <= 0:
		return fmt.Errorf("agent: tool %q embeds a tool with a Timeout, and its own spec has none: the decorator hides the timeout; implement Spec or Unwrap: %w", s.Name, ErrConfig)
	}
	return nil
}

// embeddedTool returns the tool an embedded field f holds, when it holds a non-nil one.
func embeddedTool(f reflect.Value) (Tool, bool) {
	switch {
	case f.Type().Implements(toolType):
		if (f.Kind() == reflect.Interface || f.Kind() == reflect.Pointer) && f.IsNil() {
			return nil, false
		}
		t, ok := f.Interface().(Tool)
		return t, ok
	case f.CanAddr() && reflect.PointerTo(f.Type()).Implements(toolType):
		t, ok := f.Addr().Interface().(Tool)
		return t, ok
	}
	return nil, false
}
