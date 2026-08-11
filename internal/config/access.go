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
	cur[parts[len(parts)-1]] = value

	patched, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var fresh Config
	if err := json.Unmarshal(patched, &fresh); err != nil {
		return fmt.Errorf("value does not fit config schema: %w", err)
	}
	fresh.path = c.path
	fresh.normalize()
	*c = fresh
	return nil
}
