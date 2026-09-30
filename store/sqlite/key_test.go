package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Two different steps are never one step, whatever bytes their run IDs and names hold. The
// store's in-process deduplication key must tell ("a\x00b", "c") from ("a", "b\x00c"): keyed
// alike, a concurrent Do of the second would share the first's fn and get its record back,
// though the table stores them as two rows.
func TestDo_DistinctStepsWithNULDoNotShareADo(t *testing.T) {
	for _, c := range [][4]string{{"a\x00b", "c", "a", "b\x00c"}, {"ab", "c", "a", "bc"}} {
		distinctSteps(t, c[0], c[1], c[2], c[3])
	}
}

func distinctSteps(t *testing.T, run1, name1, run2, name2 string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	release, started := make(chan struct{}), make(chan struct{})
	firstDone := make(chan agent.Record, 1)
	go func() {
		rec, err := s.Do(ctx, run1, name1, func(context.Context) (agent.Record, error) {
			close(started)
			<-release
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"first"`)}, nil
		})
		if err != nil {
			t.Error(err)
		}
		firstDone <- rec
	}()
	<-started
	ran := false
	secondDone := make(chan agent.Record, 1)
	go func() {
		rec, err := s.Do(ctx, run2, name2, func(context.Context) (agent.Record, error) {
			ran = true
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"second"`)}, nil
		})
		if err != nil {
			t.Error(err)
		}
		secondDone <- rec
	}()
	var second agent.Record
	got := false
	select {
	case second = <-secondDone:
		got = true
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	first := <-firstDone
	if !got {
		second = <-secondDone
	}
	if string(first.Result) != `"first"` {
		t.Errorf("first step's record = %s, want \"first\"", first.Result)
	}
	if !ran || string(second.Result) != `"second"` || second.Name != name2 {
		t.Errorf("second step ran=%v, record %q %s; want its own fn run and record \"second\"", ran, second.Name, second.Result)
	}
}
