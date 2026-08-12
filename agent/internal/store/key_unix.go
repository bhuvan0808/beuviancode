//go:build !windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// fileKeyProvider stores the encryption key in a 0600 file beside the state.
//
// This is the honest fallback for macOS and Linux until Phase 7 wires up Keychain
// and the Secret Service. It is weaker than DPAPI in a specific, stateable way:
// the key is not bound to the OS user, so a copied home directory carries a usable
// key with it. File permissions are the only protection.
//
// It is still meaningfully better than plaintext: the state file alone is useless,
// backups that capture only the state file cannot be decrypted, and the separation
// means a future upgrade to a real keyring changes one type rather than the
// storage format.
//
// Describe() reports this plainly, and the agent logs it at startup, because
// software that quietly provides weaker protection than a user assumes is worse
// than software that says so.
type fileKeyProvider struct {
	mu     sync.Mutex
	path   string
	cached []byte
}

// newPlatformKeyProvider returns the POSIX key provider.
func newPlatformKeyProvider(stateDir string) KeyProvider {
	return &fileKeyProvider{path: filepath.Join(stateDir, "agent.key")}
}

func (p *fileKeyProvider) Describe() string {
	return "key file with 0600 permissions (OS keyring integration lands in a later phase)"
}

// Key returns the AES key, creating one on first use.
func (p *fileKeyProvider) Key() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached != nil {
		return p.cached, nil
	}

	raw, err := os.ReadFile(p.path)
	if err == nil {
		if len(raw) != keySize {
			return nil, fmt.Errorf("key file is %d bytes, want %d", len(raw), keySize)
		}
		// A world- or group-readable key file defeats the only protection this
		// provider has, so refuse rather than continuing with a false sense of
		// security.
		if info, serr := os.Stat(p.path); serr == nil {
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				return nil, fmt.Errorf(
					"key file %s has permissions %04o; it must not be readable by other users (chmod 600)",
					p.path, perm)
			}
		}
		p.cached = raw
		return raw, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key file: %w", err)
	}

	key, err := newKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	if err := os.WriteFile(p.path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}

	p.cached = key
	return key, nil
}
