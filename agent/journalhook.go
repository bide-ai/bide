package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/internal/journalhook"
)

func init() {
	journalhook.Do = func(ctx context.Context, j any, runID, name string, fn func(context.Context) (any, error)) (any, error) {
		d, ok := j.(Durable)
		if !ok {
			return Record{}, fmt.Errorf("journalhook.Do: %T is not a journal: %w", j, ErrConfig)
		}
		return d.Do(ctx, runID, name, func(ctx context.Context) (Record, error) {
			v, err := fn(ctx)
			if err != nil {
				return Record{}, err
			}
			rec, ok := v.(Record)
			if !ok {
				return Record{}, fmt.Errorf("journalhook.Do: step %q returned %T, not an agent.Record: %w", name, v, ErrConfig)
			}
			return rec, nil
		})
	}
	journalhook.Step = func(ctx context.Context, j any, runID, name string, safety any, fn func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
		d, ok := j.(Durable)
		if !ok {
			return nil, fmt.Errorf("journalhook.Step: %T is not a journal: %w", j, ErrConfig)
		}
		if !planNodeStep(name) {
			return nil, fmt.Errorf("journalhook.Step: %q is not a plan node key: %w", name, ErrConfig)
		}
		s, ok := safety.(Safety)
		if !ok {
			return nil, fmt.Errorf("journalhook.Step: safety is %T, not an agent.Safety: %w", safety, ErrConfig)
		}
		return step(ctx, d, runID, name, fn, StepSafety(s))
	}
	journalhook.HoldStart = func(ctx context.Context, j any, runID string, start any) error {
		d, ok := j.(Durable)
		if !ok {
			return fmt.Errorf("journalhook.HoldStart: %T is not a journal: %w", j, ErrConfig)
		}
		want, ok := start.(RunStart)
		if !ok {
			return fmt.Errorf("journalhook.HoldStart: start is %T, not an agent.RunStart: %w", start, ErrConfig)
		}
		if err := checkDurable(d); err != nil {
			return err
		}
		return holdToStart(ctx, d, runID, nil, want)
	}
	journalhook.WithSalt = func(rec any, salt []byte) any {
		r := rec.(Record)
		r.salt = append([]byte(nil), salt...)
		if salt == nil {
			r.salt = nil
		}
		return r
	}
	journalhook.WithClaim = func(rec any, claim string) any {
		r := rec.(Record)
		r.claim = claim
		return r
	}
	journalhook.WithRaw = func(rec any, raw json.RawMessage) any {
		r := rec.(Record)
		r.raw = nil
		if raw != nil {
			r.raw = append([]byte(nil), raw...)
		}
		return r
	}
}
