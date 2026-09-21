package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Get returns the (secret-redacted) value at a dotted path, e.g.
// "provider.model" or "" for the whole config.
func (c *Config) Get(dottedKey string) (any, error) {
	m, err := c.RedactedMap()
	if err != nil {
		return nil, err
	}
	if dottedKey == "" {
		return m, nil
	}
	var cur any = m
	for _, part := range strings.Split(dottedKey, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%q: %q is not an object", dottedKey, part)
		}
		cur, ok = obj[part]
		if !ok {
			return nil, fmt.Errorf("no config key %q", dottedKey)
		}
	}
	return cur, nil
}

// Set applies a value at a dotted path, validates the resulting config by
// round-tripping it through the schema, and updates the receiver in place.
// The caller is responsible for calling Save.
func (c *Config) Set(dottedKey string, value any) error {
	if dottedKey == "" {
		return fmt.Errorf("key is required")
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	parts := strings.Split(dottedKey, ".")
	cur := m
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			if _, exists := cur[part]; exists {
				return fmt.Errorf("%q: %q is not an object", dottedKey, part)
			}
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	leaf := parts[len(parts)-1]
	cur[leaf] = value

	fresh, err := fromMap(m, c.path)
	if err != nil {
		// A number arriving quoted is the ordinary case, not a mistake: the
		// value a config key takes is any JSON type, so the tool schema names
		// no type for it, and a model with nothing to go on sends a string.
		// Refusing "100" for a float64 key is a tool that cannot set the key
		// it exists to set, and what the agent does next is edit the file by
		// hand. Re-read the string as JSON once and try again; a value that
		// is not JSON, or is JSON of the wrong type, keeps the first error,
		// which names what the caller actually sent.
		parsed, ok := asJSON(value)
		if !ok {
			return err
		}
		cur[leaf] = parsed
		retried, retryErr := fromMap(m, c.path)
		if retryErr != nil {
			return err
		}
		fresh = retried
	}
	*c = *fresh
	return nil
}

// fromMap round-trips a patched config map back through the schema, which is
// what validates it.
func fromMap(m map[string]any, path string) (*Config, error) {
	patched, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var fresh Config
	if err := json.Unmarshal(patched, &fresh); err != nil {
		return nil, fmt.Errorf("value does not fit config schema: %w", err)
	}
	fresh.path = path
	fresh.normalize()
	return &fresh, nil
}

// asJSON reads a string as the JSON value it spells: "100" is 100, "true" is
// true, "[\"a\"]" is a list. Anything else, including every value that is not
// a string, is reported as not spelling one.
func asJSON(value any) (any, bool) {
	s, ok := value.(string)
	if !ok {
		return nil, false
	}
	var parsed any
	if json.Unmarshal([]byte(s), &parsed) != nil {
		return nil, false
	}
	return parsed, true
}
