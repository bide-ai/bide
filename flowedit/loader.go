package flowedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"
)

// ToJSON converts a YAML flow config to the JSON plan reads, as bide check does: the document is
// decoded into plain values (a duplicate key is an error, as plan's strict JSON reading refuses
// one), every mapping key must be a string, and a second document is an error rather than
// ignored. The engine refuses what this refuses, and a test holds the two to one table.
func ToJSON(data []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var v any
	if err := dec.Decode(&v); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var next any
	switch err := dec.Decode(&next); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, err
	default:
		return nil, errors.New("more than one YAML document: a flow config is one document")
	}
	v, err := jsonKeys(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func jsonKeys(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			c, err := jsonKeys(e)
			if err != nil {
				return nil, err
			}
			x[k] = c
		}
	case map[any]any:
		return nil, errors.New("a mapping key is not a string")
	case []any:
		for i, e := range x {
			c, err := jsonKeys(e)
			if err != nil {
				return nil, err
			}
			x[i] = c
		}
	}
	return v, nil
}
