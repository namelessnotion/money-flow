package main

import "strings"

// filter decides which events are printed. Every event is still read, so the
// cursor moves past the ones it hides.
type filter struct {
	types      map[string]bool // empty: every aggregate type
	idFragment string          // lower-cased; empty: every aggregate
}

// newFilter takes a comma-separated list of aggregate types and a fragment of
// an aggregate id, either of which may be empty.
func newFilter(types, idFragment string) filter {
	f := filter{types: map[string]bool{}, idFragment: strings.ToLower(strings.TrimSpace(idFragment))}
	for _, typ := range strings.Split(types, ",") {
		if typ = strings.TrimSpace(typ); typ != "" {
			f.types[typ] = true
		}
	}
	return f
}

func (f filter) matches(e row) bool {
	if len(f.types) > 0 && !f.types[e.AggregateType] {
		return false
	}
	return f.idFragment == "" || strings.Contains(strings.ToLower(e.AggregateID), f.idFragment)
}
