//go:build !bide_audit_only

// bide view opens a local, read-only web page over a bide journal: a run list (each run's kind and
// state, filtered by state, kind and run ID), and for each run a plan flow's declared topology
// with the run's path drawn over it, or an agent run's timeline of model turns, tool calls (with
// their Safety), attempts, approvals, halts and its end state, and whether the journal verifies
// against a signed tree head or an evidence package (with approver keys, whether each m-of-n
// approval gate's signatures verify). The command is package internal/runview; this
// file plugs in the stores and the YAML decoder, which the core module does not carry.
//
//	bide view -db run.db                       # the run list of a SQLite journal
//	bide view -db run.db -run refund-42        # one run
//	bide view -postgres postgres://user:pass@host:5432/db -run refund-42
//	bide view -journal run.json                # an exported journal (audit.ExportJournal)
//	bide view -db run.db -evidence evidence.json -pubkey ed25519:<hex> -approver-keys keys.json
//	bide view -config flow.yaml                # a flow definition: its graph and problems
//
// It opens a SQLite file read-only (sqlite.OpenReadOnly) and a Postgres journal in sessions the
// server holds read-only (postgres.OpenReadOnly), and serves on a loopback address only (-addr,
// default 127.0.0.1:8484). The page needs no network: its HTML, CSS and graph are served by the
// binary.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
	"go.yaml.in/yaml/v3"
)

// stores opens each kind of journal bide view reads, read-only.
var stores = runview.Stores{
	SQLite: func(path string) (agent.Store, io.Closer, error) {
		s, err := sqlite.OpenReadOnly(path)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	},
	Postgres: func(dsn string) (agent.Store, io.Closer, error) {
		s, err := postgres.OpenReadOnly(context.Background(), dsn)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	},
	YAML: yamlToJSON,
}

// yamlToJSON converts a YAML flow config to the JSON plan reads. A YAML document is decoded into
// plain values (a duplicate key is an error, as plan's strict JSON reading refuses one), and every
// mapping key must be a string.
func yamlToJSON(data []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	v, err := jsonValue(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// jsonValue checks that every mapping in v has string keys, which JSON requires.
func jsonValue(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			c, err := jsonValue(e)
			if err != nil {
				return nil, err
			}
			x[k] = c
		}
		return x, nil
	case map[any]any:
		return nil, errors.New("a mapping key is not a string")
	case []any:
		for i, e := range x {
			c, err := jsonValue(e)
			if err != nil {
				return nil, err
			}
			x[i] = c
		}
		return x, nil
	}
	return v, nil
}
