package jsonpath

import (
	"encoding/json"
	"reflect"
	"testing"
)

const sample = `{"total": 3, "items": [{"id": 1, "label": "R1-1"}, {"id": 2, "tags": []}], "meta": null}`

func decode(t testing.TB, s string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestLookup(t *testing.T) {
	doc := decode(t, sample)
	cases := []struct {
		path   string
		want   any
		wantOK bool
	}{
		{"total", 3.0, true},
		{"items.0.id", 1.0, true},
		{"items.1.id", 2.0, true},
		{"items.-1.id", 2.0, true},
		{"items.0.label", "R1-1", true},
		{"items.1.tags", []any{}, true},
		{"meta", nil, true},
		{"items.2.id", nil, false},
		{"items.-3", nil, false},
		{"items.x", nil, false},
		{"total.deeper", nil, false},
		{"missing", nil, false},
		{"items..id", nil, false},
		{"items.0.id.", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := Lookup(doc, tc.path)
			if ok != tc.wantOK || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Lookup(%q) = %v, %v; want %v, %v", tc.path, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestEmptyPathReturnsDocument(t *testing.T) {
	doc := decode(t, `[1]`)
	got, ok := Lookup(doc, "")
	if !ok || !reflect.DeepEqual(got, doc) {
		t.Fatalf("got %v, %v", got, ok)
	}
}

// FuzzLookup checks Lookup never panics, whatever the document or path.
func FuzzLookup(f *testing.F) {
	f.Add(sample, "items.0.id")
	f.Add(`[[[]]]`, "0.0.0")
	f.Add(`{"a":{"b":[1,2,3]}}`, "a.b.-1")
	f.Add(`{}`, ".")
	f.Fuzz(func(t *testing.T, document, path string) {
		var doc any
		if json.Unmarshal([]byte(document), &doc) != nil {
			return
		}
		Lookup(doc, path)
	})
}
