// Package ids mints the identifiers every resource, operation and token
// carries, in AWS's style: a short prefix naming the kind, a dash, seventeen
// lowercase hex digits — m-0123456789abcdef0, op-…, tok-….
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

// The prefixes the core owns; plugins pick their own for their types.
const (
	Operation = "op"
	Token     = "tok"
)

var (
	prefixRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}$`)
	idRe     = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}-[0-9a-f]{17}$`)
)

// New mints an id under prefix. Seventeen hex digits are 68 random bits:
// enough that a collision is not a thing to design for, and the database's
// uniqueness constraint refuses one anyway.
func New(prefix string) string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ids: no randomness: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b[:])[:17]
}

// ValidPrefix reports whether p may prefix ids: a lowercase letter, then up
// to seven lowercase letters or digits.
func ValidPrefix(p string) bool { return prefixRe.MatchString(p) }

// Valid reports whether id is shaped like an id.
func Valid(id string) bool { return idRe.MatchString(id) }

// Prefix returns the part before the dash ("m" for m-0123…).
func Prefix(id string) string {
	p, _, _ := strings.Cut(id, "-")
	return p
}
