package hermes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

// DecodeControlRequest rejects silent overrides, duplicate keys and non-objects.
func DecodeControlRequest(raw json.RawMessage, target any) error {
	if len(raw) == 0 || len(raw) > 160<<10 {
		return errors.New("invalid control request size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 32 {
			return errors.New("control request too deeply nested")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return errors.New("duplicate control request key")
				}
				seen[key] = true
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := scan(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid control request")
		}
		_, err = d.Token()
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("control request must be an object")
	}
	if err := scan(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing control request data")
	}
	// encoding/json alone accepts case aliases and null into scalar fields.
	// The wire schema is case-sensitive and scalar fields cannot be null.
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct {
		return errors.New("control schema must be a struct")
	}
	fields := map[string]reflect.Kind{}
	for i := 0; i < typ.Elem().NumField(); i++ {
		field := typ.Elem().Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = field.Type.Kind()
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	for key, value := range object {
		kind, exists := fields[key]
		if !exists {
			return errors.New("unknown control request field")
		}
		if string(bytes.TrimSpace(value)) == "null" && kind != reflect.Pointer && kind != reflect.Map {
			return errors.New("null control request scalar")
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target)
}
