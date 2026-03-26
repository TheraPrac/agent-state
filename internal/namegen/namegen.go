// Package namegen generates human-readable identifiers using petname.
package namegen

import (
	petname "github.com/dustinkirkland/golang-petname"
)

// Generate returns a 3-word hyphen-separated identifier like "primly-lasting-toucan".
func Generate() string {
	return petname.Generate(3, "-")
}

// GenerateUnique returns an identifier not present in existing.
// Panics after 1000 retries (astronomically unlikely with trillions of combinations).
func GenerateUnique(existing []string) string {
	set := make(map[string]bool, len(existing))
	for _, id := range existing {
		set[id] = true
	}
	for i := 0; i < 1000; i++ {
		id := Generate()
		if !set[id] {
			return id
		}
	}
	panic("namegen: failed to generate unique ID after 1000 retries")
}
