package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// ResolveHalt clears a halt of a node of this flow, as agent.ResolveHaltRef does, once it has
// checked the resolution against the flow: ref must name a node of this flow (the halt's Ref: a
// node key, a loop iteration's key for a node in a loop body, or the key of a Step a node's body
// ran), the run must be a run of this flow (its recorded start names this flow, and its recorded
// topology digest is this flow's), and a successful outcome's Result must decode as the node's
// output type, so the next Run can feed it downstream. A resolution agent.ResolveHaltRef records is final, so one the flow
// cannot read would leave the run unable to continue; ResolveHalt refuses it (ErrConfig) instead
// and records nothing. A Step a node's body ran returns a type the flow does not declare, so its
// Result is not checked.
//
//	if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
//	    // confirm out of band that the charge went through, then record the node's output
//	    err = flow.ResolveHalt(ctx, store, halt.Ref(), agent.Outcome{Result: receipt})
//	}
func (f *Flow[In, Out]) ResolveHalt(ctx context.Context, store agent.Durable, ref agent.HaltRef, out agent.Outcome, opts ...agent.ResolveOption) error {
	c := f.core
	if ref.Op.Kind != agent.OpStep {
		return fmt.Errorf("plan: flow %q: resolve %s %q: a flow's halts are on Steps (%q): %w", c.flowName, ref.Op.Kind, ref.Op.ID, agent.OpStep, agent.ErrConfig)
	}
	name, iter, ok := parseNodeKey(ref.Op.ID)
	if !ok || c.byName[name] == nil {
		return fmt.Errorf("plan: flow %q: resolve %q: not a node of this flow: %w", c.flowName, ref.Op.ID, agent.ErrConfig)
	}
	inLoop := false
	for _, lp := range c.loops {
		inLoop = inLoop || slices.Contains(lp.body, name)
	}
	if (iter >= 0) != inLoop {
		return fmt.Errorf("plan: flow %q: resolve %q: node %q is in a loop body iff its key names an iteration: %w", c.flowName, ref.Op.ID, name, agent.ErrConfig)
	}
	if err := c.checkRunOfFlow(ctx, store, ref.RunID); err != nil {
		return err
	}
	rest, _ := splitIter(strings.TrimPrefix(ref.Op.ID, "node:"))
	if nodeOwn := !strings.Contains(rest, ":"); nodeOwn && !out.IsError {
		b, err := journalhook.Marshal(out.Result) // as agent.ResolveHaltRef will record it
		if err != nil {
			return fmt.Errorf("plan: flow %q: resolve %q: encode the outcome: %w (%w)", c.flowName, ref.Op.ID, err, agent.ErrConfig)
		}
		if err := decodeStrict(b, c.byName[name].outType); err != nil {
			return fmt.Errorf("plan: flow %q: resolve %q: the outcome %s is not a %s, node %q's output: %w (%w)", c.flowName, ref.Op.ID, b, typeName(c.byName[name].outType), name, err, agent.ErrConfig)
		}
	}
	return agent.ResolveHaltRef(ctx, store, ref, out, opts...)
}

// decodeStrict decodes raw into a fresh value of type into as Run will decode a node's recorded
// output, and refuses what that decode would accept without taking it: a field the type does not
// have, trailing data, and null for a type that cannot be nil.
func decodeStrict(raw []byte, into reflect.Type) error {
	if into == nil {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		switch into.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
			return nil
		}
		return fmt.Errorf("null is not a %s", typeName(into))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(reflect.New(into).Interface()); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after the value")
	}
	return nil
}

// checkRunOfFlow refuses (ErrConfig) a run that is not a run of c: one whose recorded start is not
// a flow run of c's name, or whose recorded topology digest is not c's. A resolution recorded
// through another flow, even one with a node of the same name, would record a value of that
// flow's node type, which the run's own flow may not be able to read.
func (c *builderCore) checkRunOfFlow(ctx context.Context, store agent.Durable, runID string) error {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return fmt.Errorf("plan: flow %q: read run %s: %w", c.flowName, runID, err)
	}
	var start *agent.RunStart
	digest := ""
	for _, r := range recs {
		switch r.Name {
		case runStartStep:
			var st agent.RunStart
			if json.Unmarshal(r.Result, &st) == nil {
				start = &st
			}
		case flowDigestStep:
			_ = json.Unmarshal(r.Result, &digest)
		}
	}
	if start == nil || start.Kind != agent.RunKindFlow || start.Flow == nil || start.Flow.Name != c.flowName {
		return fmt.Errorf("plan: flow %q: run %s is not a run of this flow (see agent.RecordedStart): %w", c.flowName, runID, agent.ErrConfig)
	}
	if digest != c.digest() {
		return fmt.Errorf("plan: flow %q: run %s was recorded under topology digest %q, not this flow's %s: %w", c.flowName, runID, digest, c.digest(), agent.ErrConfig)
	}
	return nil
}
