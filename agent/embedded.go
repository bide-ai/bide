package agent

import (
	"fmt"
	"reflect"
	"slices"
	"unsafe"
)

var toolType = reflect.TypeFor[Tool]()

// checkEmbedded refuses a tool that hides the approval gate, timeout or amount caps of a tool it embeds. A
// decorator that embeds a Tool inherits its Spec, gate and timeout included; one that declares its
// own Spec and builds the ToolSpec afresh (to rename the tool, say) drops them, and the gated tool
// inside would run ungated. The agent cannot know whether a decorator meant to drop them, so it
// fails closed: when any tool embedded in t (an anonymous field, at any depth) has an Approval, a
// Timeout or a cap rule that t's own spec s lacks (for caps: any rule line the inner tool has and s
// does not), the tool is ErrConfig. A decorator keeps them by declaring them in its Spec.
// Implementing Unwrap() Tool does not exempt it: New does not read the spec through Unwrap, the
// outer Spec wins, and checkUnwrapChain holds that spec to every tool on the chain.
func checkEmbedded(t Tool, s ToolSpec) error {
	var approval, timeout bool
	var caps []string // the cap rule lines of the embedded tools
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
				caps = append(caps, is.Caps...)
			}
			walk(f, depth+1)
		}
	}
	walk(reflect.ValueOf(t), 0)
	switch {
	case approval && s.Approval == nil:
		return fmt.Errorf("agent: tool %q embeds a tool with an approval gate, and its own spec has none: the decorator hides the approval gate; implement Spec or Unwrap: %w", s.Name, ErrConfig)
	case slices.ContainsFunc(caps, func(l string) bool { return !slices.Contains(s.Caps, l) }):
		return fmt.Errorf("agent: tool %q embeds a tool with amount caps, and its own spec lacks or changes a rule: the decorator hides the cap; implement Spec (carrying the same rules) or Unwrap: %w", s.Name, ErrConfig)
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

// checkUnwrapChain refuses a wrapper whose own spec s weakens the approval gate or the timeout of a
// tool on its Unwrap chain. checkEmbedded sees only anonymous fields; a decorator that holds the
// inner tool in a named field, implements Spec itself and implements Unwrap() Tool passes it, and
// the outer Spec wins, so a gate the inner tool declared would silently not apply. Every tool the
// chain reaches is compared with s, and the first weaker field is ErrConfig, naming the field and
// the tool. The remedy is to declare the same on the wrapper (or take the spec from the wrapped tool).
//
// Weaker, per field:
//   - Approval: s has none; or s needs fewer decisions than the inner gate (a SingleApproval needs
//     one); or the inner gate is m-of-n and s is a SingleApproval (no named approvers); or s
//     admits an approver the inner gate does not name (a wider pool); or the inner gate has
//     ExcludePrincipal and s does not; or the inner gate has a deadline (Approval.Timeout) and s has
//     none or a longer one.
//   - Timeout: the inner tool has one and s has none or a longer one.
//   - Caps: a rule line (ToolSpec.Caps) of the inner tool that s does not carry, word for word. s may add
//     rules; a rule it changes is a missing one.
func checkUnwrapChain(t Tool, s ToolSpec) error {
	for range 64 { // the same bound as asSubAgent
		u, wraps := t.(interface{ Unwrap() Tool })
		if !wraps {
			return nil
		}
		if t = u.Unwrap(); isNil(t) {
			return nil
		}
		is := specOf(t)
		if field, why := weakerSpec(s, is); field != "" {
			return fmt.Errorf("agent: tool %q wraps %q (Unwrap) and weakens its %s: %s; declare the same %s on the wrapper: %w", s.Name, is.Name, field, why, field, ErrConfig)
		}
	}
	return nil // a chain longer than 64 is refused by checkWrapper
}

// weakerSpec reports the first field of outer that is weaker than the same field of inner, and why.
func weakerSpec(outer, inner ToolSpec) (field, why string) {
	if ia := inner.Approval; ia != nil {
		oa := outer.Approval
		switch {
		case oa == nil:
			return "approval gate", "the wrapper has none"
		case oa.Need < ia.Need:
			return "approval gate", fmt.Sprintf("the wrapper needs %d decisions, the wrapped tool %d", oa.Need, ia.Need)
		case !ia.one && oa.one:
			return "approval gate", "the wrapper is a SingleApproval, the wrapped tool's gate is m-of-n over named approvers"
		case !ia.one && !oa.one && !subset(oa.Approvers, ia.Approvers):
			return "approval gate", "the wrapper admits approvers the wrapped tool's gate does not name"
		case ia.ExcludePrincipal && !oa.ExcludePrincipal:
			return "approval gate", "the wrapped tool's gate has ExcludePrincipal and the wrapper's does not"
		case ia.Timeout > 0 && (oa.Timeout <= 0 || oa.Timeout > ia.Timeout):
			return "approval gate", fmt.Sprintf("the wrapped tool's gate expires after %v, the wrapper's after %v (0 is never)", ia.Timeout, oa.Timeout)
		}
	}
	for _, l := range inner.Caps {
		if !slices.Contains(outer.Caps, l) {
			return "cap rules", fmt.Sprintf("the wrapper lacks the rule %q (a rule line is compared as written, so a changed line counts as missing)", l)
		}
	}
	if inner.Timeout > 0 && (outer.Timeout <= 0 || outer.Timeout > inner.Timeout) {
		return "timeout", fmt.Sprintf("the wrapped tool's is %v, the wrapper's %v (0 is none)", inner.Timeout, outer.Timeout)
	}
	return "", ""
}

// subset reports whether every element of a is in b.
func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}
