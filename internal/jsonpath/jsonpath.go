// Package jsonpath looks up values in decoded JSON with simple dotted paths:
// "total", "items.0.id", "items.-1.label" (negative indexes count from the end).
package jsonpath

import (
	"strconv"
	"strings"
)

// Lookup walks doc (as produced by encoding/json) along path.
// It reports false if any segment is missing or the wrong type.
// An empty path returns doc itself.
func Lookup(doc any, path string) (any, bool) {
	if path == "" {
		return doc, true
	}
	current := doc
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return nil, false
		}
		switch node := current.(type) {
		case map[string]any:
			value, ok := node[segment]
			if !ok {
				return nil, false
			}
			current = value
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil {
				return nil, false
			}
			if index < 0 {
				index += len(node)
			}
			if index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}
