// Package escalation names the subject of the escalation gate instance of a tool call or a plan flow node
// (docs/design/untrusted-content-and-amount-caps.md, section 5.3, and docs/design/flow-taint.md), for the
// packages of this module that must tell it from the call or node itself: the agent engine, which asks the
// instance, and the approvals intake, which must not decide it under the call's own gate. It is internal so
// that no new exported name is needed.
//
// The instance of a tool call is its ID and "#taint": a real tool-use ID never carries it, because a model
// turn with a '#' in an ID is refused (agent.ErrToolUseIDInvalid). The instance of a plan flow node is its key
// and ":taint" ("node:x:taint", "node:iter:2:x:taint"): a node name holds no ':' (package plan refuses
// one), so no node can be named so, and the form is not a plan key, so it collides with no node and no Step
// a node's body runs. A node whose name contains '#' (which plan allows) is a node, never an instance.
package escalation

import "strings"

// Suffix turns a tool call's ID into the subject of its escalation instance.
const Suffix = "#taint"

// NodeSuffix turns a plan flow node's key into the subject of its escalation instance.
const NodeSuffix = ":taint"

const (
	planPrefix = "node:"
	stepSep    = ":step:"
)

// PlanKey is the one parser of the keys package plan runs its nodes under: "node:[iter:<n>:]<node>", and
// "node:[iter:<n>:]<node>:step:<step>" for a Step a node's body runs. It returns the node's key and, for a
// Step, the Step's name ("" for a node). ok is false for any other name, the instance of a node included.
// <n> is a decimal number with no leading zero (the form strconv.Itoa writes), "iter:" followed by anything
// else is a node named iter, and a node name is not empty and holds no ':'.
func PlanKey(name string) (node, step string, ok bool) {
	rest, ok := strings.CutPrefix(name, planPrefix)
	if !ok {
		return "", "", false
	}
	body := rest
	if it, ok := strings.CutPrefix(rest, "iter:"); ok {
		if digits, after, ok := strings.Cut(it, ":"); ok && isIterNumber(digits) {
			body = after
		}
	}
	n, tail, scoped := strings.Cut(body, ":")
	if n == "" {
		return "", "", false
	}
	nodeKey := name[:len(name)-len(body)] + n
	if !scoped {
		return nodeKey, "", true
	}
	step, ok = strings.CutPrefix(":"+tail, stepSep)
	if !ok || step == "" {
		return "", "", false
	}
	return nodeKey, step, true
}

// isIterNumber reports whether s is a loop iteration as package plan writes it: decimal digits with no
// leading zero, or "0".
func isIterNumber(s string) bool {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// IsNodeKey reports whether name is the key of a plan flow node (not of a Step its body runs).
func IsNodeKey(name string) bool {
	_, step, ok := PlanKey(name)
	return ok && step == ""
}

// ID is the subject of the escalation gate instance of the call or plan flow node.
func ID(call string) string {
	if IsNodeKey(call) {
		return call + NodeSuffix
	}
	return call + Suffix
}

// Base is the call or plan flow node a gate subject belongs to, and whether the subject is an escalation
// instance. A node's key is a node, not an instance, whatever it ends with: "node:taint" is the node named
// taint, and "node:x:step:taint" a Step named taint.
func Base(subject string) (string, bool) {
	if strings.HasPrefix(subject, planPrefix) {
		if _, _, isKey := PlanKey(subject); isKey {
			return "", false
		}
		if node, ok := strings.CutSuffix(subject, NodeSuffix); ok && IsNodeKey(node) {
			return node, true
		}
		return "", false
	}
	return strings.CutSuffix(subject, Suffix)
}

// IsSubject reports whether subject is an escalation instance, not a call or a node.
func IsSubject(subject string) bool {
	_, ok := Base(subject)
	return ok
}
