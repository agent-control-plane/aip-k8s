package main

import (
	"encoding/json"
	"fmt"
	"slices"
)

var allowedSchemaKeywords = []string{"type", "properties", "items", "required", "nullable", "description"}
var allowedTypes = []string{"object", "string", "integer", "number", "boolean", "array"}

func validateContextSchema(raw []byte) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return validateSubSchema(m)
}

func validateSubSchema(m map[string]any) error {
	for k, v := range m {
		if !slices.Contains(allowedSchemaKeywords, k) {
			return fmt.Errorf("disallowed keyword: %q", k)
		}
		if k == "type" {
			t, ok := v.(string)
			if !ok || !slices.Contains(allowedTypes, t) {
				return fmt.Errorf("disallowed or invalid type: %v", v)
			}
		}
		if k == "properties" {
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("properties must be an object")
			}
			for propName, propVal := range props {
				propSchema, ok := propVal.(map[string]any)
				if !ok {
					return fmt.Errorf("property %q must be a schema object", propName)
				}
				if err := validateSubSchema(propSchema); err != nil {
					return fmt.Errorf("property %q: %w", propName, err)
				}
			}
		}
		if k == "items" {
			itemsSchema, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("items must be a schema object")
			}
			if err := validateSubSchema(itemsSchema); err != nil {
				return fmt.Errorf("items: %w", err)
			}
		}
	}
	return nil
}
