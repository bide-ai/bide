// Package durabletest is the record-fidelity suite under its former name.
//
// Deprecated: transitional; removed by the 1.0 rewrite. Use package storetest: storetest.Run for a
// Store, storetest.RunDurable for a Durable.
package durabletest

import (
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/storetest"
)

// Run runs the record-fidelity suite against a Durable (see storetest.RunDurable).
//
// Deprecated: transitional; removed by the 1.0 rewrite. Use storetest.RunDurable, or storetest.Run
// for a Store.
func Run(t *testing.T, open func(t *testing.T) agent.Durable) { storetest.RunDurable(t, open) }

// Case is one record the suite round-trips (see storetest.Case).
//
// Deprecated: transitional; removed by the 1.0 rewrite. Use storetest.Case.
type Case = storetest.Case

// Cases returns the records the suite round-trips (see storetest.Cases).
//
// Deprecated: transitional; removed by the 1.0 rewrite. Use storetest.Cases.
func Cases() []Case { return storetest.Cases() }
