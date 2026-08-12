package store

import "path/filepath"

// Open returns a Store for the given state file path, using the platform's key
// protection.
//
// The single constructor callers should use: it selects DPAPI on Windows and the
// file-based provider elsewhere at compile time, so no caller has to know which
// platform it is on.
func Open(statePath string) *Store {
	return New(statePath, newPlatformKeyProvider(filepath.Dir(statePath)))
}

// OpenWithKey returns a Store using a caller-supplied key provider.
//
// For tests, which must not touch the real OS keyring — a test that writes to a
// developer's Keychain is a test nobody wants to run twice.
func OpenWithKey(statePath string, keyFn KeyProvider) *Store {
	return New(statePath, keyFn)
}

// StaticKey is a fixed-key provider for tests.
type StaticKey [32]byte

// Key returns the fixed key.
func (k StaticKey) Key() ([]byte, error) { return k[:], nil }

// Describe names the provider.
func (k StaticKey) Describe() string { return "static test key (INSECURE)" }
