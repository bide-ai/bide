package plan

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// loadAny loads data against registry sel with that registry's In/Out types and returns the
// digest, and a function that runs the flow on a small input.
func loadAny(t *testing.T, sel int, data []byte) (string, func(context.Context) error, error) {
	switch sel {
	case 0:
		f, err := Load[cfgOrder, cfgReceipt](data, triageRegistry(t))
		if err != nil {
			return "", nil, err
		}
		return f.Digest(), func(ctx context.Context) error {
			_, err := f.Run(ctx, agent.NewMemStore(), "r", cfgOrder{ID: 1, Rush: true})
			return err
		}, nil
	case 1:
		f, err := Load[int, string](data, diamondRegistry(t))
		if err != nil {
			return "", nil, err
		}
		return f.Digest(), func(ctx context.Context) error {
			_, err := f.Run(ctx, agent.NewMemStore(), "r", 3)
			return err
		}, nil
	default:
		f, err := Load[int, string](data, loopRegistry(t))
		if err != nil {
			return "", nil, err
		}
		return f.Digest(), func(ctx context.Context) error {
			_, err := f.Run(ctx, agent.NewMemStore(), "r", 3)
			return err
		}, nil
	}
}

func fuzzRegistry(t *testing.T, sel int) *Registry {
	switch sel {
	case 0:
		return triageRegistry(t)
	case 1:
		return diamondRegistry(t)
	default:
		return loopRegistry(t)
	}
}

// FuzzLoadConfig: arbitrary config bytes never panic Load or Validate; anything that loads also
// validates, has a stable Digest across loads, has the same Digest after a canonical
// re-serialization of the parsed config, and (with small loop bounds) runs to completion.
func FuzzLoadConfig(f *testing.F) {
	f.Add(uint8(0), []byte(triageConfig))
	f.Add(uint8(1), []byte(diamondConfig))
	f.Add(uint8(2), []byte(loopConfig))
	f.Add(uint8(2), []byte(`{"flow":"x","entry":"seed","nodes":[{"name":"seed","block":"seed"}],"wiring":[{"switch":"seed","when":[{"pred":"again","to":"seed","loopMax":1}]}]}`))
	// A duplicate name, a case-variant name, an unknown name, a lone surrogate escape, and trailing
	// data: a config must load as it reads.
	f.Add(uint8(1), []byte(strings.Replace(diamondConfig, `"flow":`, `"flow":"other","flow":`, 1)))
	f.Add(uint8(1), []byte(strings.Replace(diamondConfig, `"flow":`, `"Flow":`, 1)))
	f.Add(uint8(1), []byte(strings.Replace(diamondConfig, `"flow":`, `"approved_by":"cfo","flow":`, 1)))
	f.Add(uint8(1), []byte(strings.Replace(diamondConfig, `"flow": "`, `"flow":"\ud800`, 1)))
	f.Add(uint8(1), []byte(diamondConfig+` {}`))
	f.Fuzz(func(t *testing.T, sel8 uint8, data []byte) {
		sel := int(sel8) % 3
		verr := Validate(data, fuzzRegistry(t, sel))
		d1, run, err := loadAny(t, sel, data)
		if err != nil {
			return
		}
		if verr != nil {
			t.Fatalf("Load succeeded but Validate failed: %v\n%s", verr, data)
		}
		d2, _, err := loadAny(t, sel, data)
		if err != nil || d1 != d2 {
			t.Fatalf("second Load differs: %v, %s vs %s", err, d1, d2)
		}
		var cfg config
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("loaded config does not re-parse: %v", err)
		}
		canon, _ := json.Marshal(cfg)
		d3, _, err := loadAny(t, sel, canon)
		if err != nil || d3 != d1 {
			t.Fatalf("canonical re-serialization loads differently (%v): %s vs %s\norig:  %s\ncanon: %s", err, d1, d3, data, canon)
		}
		for _, w := range cfg.Wiring {
			for _, a := range w.When {
				if a.LoopMax > 50 {
					return
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- run(ctx) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("loaded flow did not finish or honor cancellation: %s", data)
		}
	})
}
