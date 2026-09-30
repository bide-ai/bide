package audit_test

import (
	"reflect"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// withSalt returns r with its salt replaced by salt: a record no journal writes.
func withSalt(r agent.Record, salt []byte) agent.Record {
	return journalhook.WithSalt(r, salt).(agent.Record)
}

// stripRaw clears the stored bytes (agent.Record.Raw) of every record reachable from v, a pointer:
// a record read from a journal carries them, and one decoded from an artifact's JSON does not.
func stripRaw(v any) {
	stripRawValue(reflect.ValueOf(v))
}

func stripRawValue(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			stripRawValue(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[agent.Record]() {
			if v.CanSet() {
				v.Set(reflect.ValueOf(journalhook.WithRaw(v.Interface().(agent.Record), nil)))
			}
			if m := v.FieldByName("Message"); m.IsValid() {
				stripRawValue(m)
			}
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				stripRawValue(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			stripRawValue(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(k))
			stripRawValue(e)
			v.SetMapIndex(k, e)
		}
	}
}
