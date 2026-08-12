package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/id"
)

// State is everything the agent persists between runs.
type State struct {
	// DeviceID is assigned by the backend at registration. Empty means this
	// installation has never registered.
	DeviceID string `json:"device_id"`

	// DeviceToken authenticates the WebSocket. The reason this file is encrypted.
	DeviceToken string    `json:"device_token"`
	TokenExpiry time.Time `json:"token_expiry"`

	// UserID is stored for display only; the token is the actual credential.
	UserID string `json:"user_id"`

	// PendingPrompts are prompts received but not yet injected, kept so a prompt
	// sent from a phone survives an agent restart rather than vanishing.
	PendingPrompts []PendingPrompt `json:"pending_prompts"`

	// LastSessionID lets a restarted agent reattach to a session the backend
	// still believes is running, instead of orphaning it.
	LastSessionID string `json:"last_session_id"`

	UpdatedAt time.Time `json:"updated_at"`
}

// PendingPrompt is a prompt awaiting injection.
type PendingPrompt struct {
	PromptID   string    `json:"prompt_id"`
	Text       string    `json:"text"`
	ReceivedAt time.Time `json:"received_at"`
	Attempts   int       `json:"attempts"`
}

// Registered reports whether this installation has credentials.
func (s *State) Registered() bool { return s.DeviceID != "" && s.DeviceToken != "" }

// TokenExpiringSoon reports whether the device token should be refreshed.
//
// A week of margin: the agent may be offline for days, and discovering an expired
// token at the moment the user needs it is the worst possible time.
func (s *State) TokenExpiringSoon(now time.Time) bool {
	if s.TokenExpiry.IsZero() {
		return false
	}
	return now.Add(7 * 24 * time.Hour).After(s.TokenExpiry)
}

// Store persists State encrypted at rest.
//
// Safe for concurrent use: the session manager queues prompts while the transport
// updates tokens.
type Store struct {
	mu    sync.Mutex
	path  string
	state State
	keyFn KeyProvider
}

// KeyProvider supplies the encryption key.
//
// An interface so the platform-specific secret store (DPAPI, Keychain, Secret
// Service) is swappable and testable without touching the OS keyring in tests.
type KeyProvider interface {
	// Key returns a 32-byte AES-256 key, creating and persisting one on first use.
	Key() ([]byte, error)
	// Describe names the protection in use, for the startup log. Users deserve to
	// know whether their credentials are OS-protected or only file-permission
	// protected.
	Describe() string
}

// New opens or creates a store at path.
func New(path string, keyFn KeyProvider) *Store {
	return &Store{path: path, keyFn: keyFn}
}

// ErrCorrupt means the state file could not be decrypted or parsed.
//
// Distinguished from a missing file because the responses differ: a missing file
// means "register normally", while a corrupt one means credentials are
// unrecoverable and the user must re-register.
var ErrCorrupt = errors.New("store: state file is unreadable")

// Load reads and decrypts the state file.
//
// A missing file yields empty state and no error: that is a fresh install, not a
// failure.
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.state = State{}
			return s.state, nil
		}
		return State{}, fmt.Errorf("store: read %s: %w", s.path, err)
	}
	if len(raw) == 0 {
		s.state = State{}
		return s.state, nil
	}

	key, err := s.keyFn.Key()
	if err != nil {
		return State{}, fmt.Errorf("store: obtain encryption key: %w", err)
	}

	plaintext, err := decrypt(key, raw)
	if err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}

	var state State
	if err := json.Unmarshal(plaintext, &state); err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	s.state = state
	return state, nil
}

// Save encrypts and writes the state atomically.
func (s *Store) Save(state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state.UpdatedAt = time.Now().UTC()
	s.state = state
	return s.persist(state)
}

// Update applies fn to the current state and saves the result.
//
// Read-modify-write under one lock, so two goroutines updating different fields
// cannot clobber each other — which a Load/mutate/Save sequence at call sites
// absolutely would.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.state
	fn(&next)
	next.UpdatedAt = time.Now().UTC()
	s.state = next
	return s.persist(next)
}

// Current returns a copy of the in-memory state.
func (s *Store) Current() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// persist writes the state atomically. Caller must hold the lock.
//
// Write-to-temp-then-rename, with an fsync before the rename. A partial write
// during a crash or power loss would otherwise leave an unparseable file, and
// recovering from that means re-registering the device and losing every queued
// prompt — exactly the data this store exists to protect.
func (s *Store) persist(state State) error {
	dir := filepath.Dir(s.path)
	// 0700: the directory holds credentials and should not be listable by other
	// users of a shared machine.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}

	plaintext, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("store: encode state: %w", err)
	}

	key, err := s.keyFn.Key()
	if err != nil {
		return fmt.Errorf("store: obtain encryption key: %w", err)
	}
	ciphertext, err := encrypt(key, plaintext)
	if err != nil {
		return fmt.Errorf("store: encrypt state: %w", err)
	}

	// The temporary file must be in the SAME directory as the target: rename is
	// only atomic within a filesystem, and /tmp is frequently a different one.
	tmp, err := os.CreateTemp(dir, ".agent-state-*.tmp")
	if err != nil {
		return fmt.Errorf("store: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails; harmless once renamed.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: set permissions: %w", err)
	}
	if _, err := tmp.Write(ciphertext); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: write state: %w", err)
	}
	// fsync before rename: without it the rename can land while the contents are
	// still in the page cache, which is how a crash produces a zero-length file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close temp file: %w", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("store: replace state file: %w", err)
	}
	return nil
}

// Reset clears the state, used when credentials are rejected and the device must
// re-register.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = State{}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: remove state file: %w", err)
	}
	return nil
}

// Path returns the state file location, for logging.
func (s *Store) Path() string { return s.path }

// Protection describes the key protection in use.
func (s *Store) Protection() string { return s.keyFn.Describe() }

// ---------------------------------------------------------------------------
// Encryption
//
// AES-256-GCM: authenticated encryption, so a tampered file fails to decrypt
// rather than yielding attacker-chosen plaintext. Plain AES-CTR or CBC would
// encrypt but not authenticate, which for a credential store means an attacker
// who can write the file can flip bits in the token.

const (
	keySize   = 32 // AES-256
	nonceSize = 12 // GCM standard nonce size
)

func encrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// A fresh random nonce per write. Reusing a nonce with the same key is
	// catastrophic for GCM — it leaks the XOR of the plaintexts and allows
	// forgery — so this must never be derived from anything predictable.
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	// Nonce is prepended; it is not secret, only unique.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(key, ciphertext []byte) ([]byte, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext is shorter than the nonce")
	}

	nonce, body := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		// Deliberately vague: distinguishing "wrong key" from "tampered data"
		// would be an oracle. The caller only needs to know it is unusable.
		return nil, errors.New("decryption failed")
	}
	return plaintext, nil
}

// newKey generates a fresh AES-256 key.
func newKey() ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return key, nil
}

// NewNonce returns a random identifier, re-exported for the transport's AUTH
// frames so callers need not import shared/id separately.
func NewNonce() string { return id.Nonce() }
