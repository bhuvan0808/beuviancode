// Package id mints sortable, collision-resistant identifiers.
//
// IDs are ULID-compatible: a 48-bit millisecond timestamp followed by 80 bits of
// cryptographic randomness, rendered in Crockford base32 as 26 characters.
//
// ULID rather than UUIDv4 because the leading timestamp makes IDs
// lexicographically sortable by creation time. That matters concretely here: it
// gives PostgreSQL B-tree indexes on our primary keys good locality (appends land
// at the right edge of the index instead of scattering random pages), and it lets
// the dashboard order log lines and sessions by ID alone.
//
// Implemented against crypto/rand and the standard library only, to preserve the
// shared module's zero-dependency invariant.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// crockford is Crockford base32: digits plus uppercase letters with I, L, O, and
// U removed, so an ID cannot be misread or mistyped between 1/I/L or 0/O. Users
// read device IDs out of logs and support tickets, so this is worth the trouble.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Length is the character length of a generated ID, excluding any prefix.
const Length = 26

const (
	timeChars   = 10 // 48-bit timestamp, encoded in the first 10 characters
	randomBytes = 10 // 80 bits of randomness
)

// Entity prefixes. A prefixed ID is self-describing, so a value that leaks into
// a log line or an error message announces what it is, and passing a session ID
// where a device ID belongs is visible on inspection rather than silent.
const (
	PrefixUser         = "usr"
	PrefixDevice       = "dev"
	PrefixSession      = "ses"
	PrefixRepository   = "rep"
	PrefixPrompt       = "prm"
	PrefixMessage      = "msg"
	PrefixNotification = "ntf"
	PrefixLog          = "log"
	PrefixCorrelation  = "cor"
	PrefixRequest      = "req"
)

// New returns an unprefixed 26-character sortable ID.
//
// It panics only if the system CSPRNG fails, which on a supported platform means
// the OS is in an unrecoverable state. Returning an error here would force error
// handling into every call site (including struct literals) to guard against a
// condition no caller could sensibly recover from.
func New() string {
	return NewAt(time.Now())
}

// NewAt is New with an explicit timestamp, for deterministic tests.
func NewAt(t time.Time) string {
	var buf [Length]byte

	ms := uint64(t.UTC().UnixMilli())
	// Encode the low 50 bits of the timestamp across 10 five-bit groups. A
	// 48-bit millisecond counter does not overflow until the year 10889.
	for i := timeChars - 1; i >= 0; i-- {
		buf[i] = crockford[ms&31]
		ms >>= 5
	}

	var rnd [randomBytes]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic(fmt.Sprintf("id: system CSPRNG unavailable: %v", err))
	}
	// 80 bits divides evenly into 16 five-bit groups, so there is no padding.
	var acc uint32
	var bits uint
	out := timeChars
	for _, b := range rnd {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			buf[out] = crockford[(acc>>bits)&31]
			out++
		}
	}

	return string(buf[:])
}

// WithPrefix returns a prefixed ID such as "dev_01J9Z3K7QF8XKM2N4P6R8T0VWY".
func WithPrefix(prefix string) string {
	return prefix + "_" + New()
}

// ErrInvalid reports a malformed identifier.
var ErrInvalid = errors.New("id: invalid identifier")

// decodeMap inverts crockford for validation and timestamp recovery.
var decodeMap = func() [256]int8 {
	var m [256]int8
	for i := range m {
		m[i] = -1
	}
	for i, c := range crockford {
		m[byte(c)] = int8(i)
	}
	return m
}()

// Validate checks that s is a well-formed ID, with or without a prefix.
//
// Used at trust boundaries — an ID arriving from a request path or a WebSocket
// payload is validated before it reaches a database query, so a malformed value
// fails fast with a clear error rather than as an opaque driver error.
func Validate(s string) error {
	body := s
	if i := strings.LastIndex(s, "_"); i >= 0 {
		if i == 0 || i == len(s)-1 {
			return fmt.Errorf("%w: malformed prefix in %q", ErrInvalid, s)
		}
		body = s[i+1:]
	}
	if len(body) != Length {
		return fmt.Errorf("%w: want %d characters, got %d", ErrInvalid, Length, len(body))
	}
	for i := 0; i < len(body); i++ {
		if decodeMap[body[i]] < 0 {
			return fmt.Errorf("%w: illegal character %q at %d", ErrInvalid, body[i], i)
		}
	}
	return nil
}

// Time recovers the creation timestamp encoded in an ID.
//
// Useful for expiry checks and for debugging without a database round trip.
func Time(s string) (time.Time, error) {
	if err := Validate(s); err != nil {
		return time.Time{}, err
	}
	body := s
	if i := strings.LastIndex(s, "_"); i >= 0 {
		body = s[i+1:]
	}
	// Decoded values are validated to be in 0..31 before Time is called, and the
	// timestamp only ever reaches 48 bits, so int64 arithmetic cannot overflow.
	// Widening signed-to-signed keeps the conversion unambiguous (gosec G115).
	var ms int64
	for i := 0; i < timeChars; i++ {
		ms = ms<<5 | int64(decodeMap[body[i]])
	}
	return time.UnixMilli(ms).UTC(), nil
}

// Nonce returns a 128-bit random value for replay protection.
//
// Distinct from New: a nonce must be unpredictable with no embedded timestamp,
// because it is used as a single-use challenge value.
func Nonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("id: system CSPRNG unavailable: %v", err))
	}
	var sb strings.Builder
	sb.Grow(26)
	var acc uint32
	var bits uint
	for _, by := range b {
		acc = acc<<8 | uint32(by)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(crockford[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		sb.WriteByte(crockford[(acc<<(5-bits))&31])
	}
	return sb.String()
}

// Uint64 returns a cryptographically random uint64, for jitter and tie-breaking.
func Uint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("id: system CSPRNG unavailable: %v", err))
	}
	return binary.BigEndian.Uint64(b[:])
}
