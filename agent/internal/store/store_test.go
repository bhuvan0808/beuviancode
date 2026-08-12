package store_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/agent/internal/store"
)

// testKey avoids touching the real OS keyring: a test that writes to a
// developer's Keychain or DPAPI store is one nobody wants to run twice.
var testKey = store.StaticKey{
	1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
}

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.state")
	return store.OpenWithKey(path, testKey), path
}

func TestFreshInstallLoadsEmptyState(t *testing.T) {
	// A missing file is a fresh install, not a failure. Erroring here would make
	// first run indistinguishable from corruption.
	s, _ := newStore(t)

	state, err := s.Load()
	if err != nil {
		t.Fatalf("Load on a fresh install should succeed: %v", err)
	}
	if state.Registered() {
		t.Error("a fresh install must not report itself registered")
	}
}

func TestRoundTripPreservesCredentialsAndQueue(t *testing.T) {
	s, _ := newStore(t)

	want := store.State{
		DeviceID:      "dev_01J9Z3K7QF8XKM2N4P6R8T0VWY",
		DeviceToken:   "eyJhbGciOiJIUzI1NiJ9.payload.signature",
		TokenExpiry:   time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Second),
		UserID:        "usr_01J9Z3K7QF8XKM2N4P6R8T0VWY",
		LastSessionID: "ses_01J9Z3K7QF8XKM2N4P6R8T0VWY",
		PendingPrompts: []store.PendingPrompt{
			{PromptID: "prm_1", Text: "Now implement authentication.", ReceivedAt: time.Now().UTC().Truncate(time.Second)},
		},
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A brand-new Store, to prove the data survives a process restart rather than
	// merely being cached in memory.
	reopened := store.OpenWithKey(s.Path(), testKey)
	got, err := reopened.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.DeviceID != want.DeviceID || got.DeviceToken != want.DeviceToken {
		t.Errorf("credentials did not survive: got %+v", got)
	}
	if !got.Registered() {
		t.Error("restored state should report registered")
	}
	if len(got.PendingPrompts) != 1 || got.PendingPrompts[0].Text != want.PendingPrompts[0].Text {
		t.Errorf("queued prompts did not survive: %+v", got.PendingPrompts)
	}
	if !got.TokenExpiry.Equal(want.TokenExpiry) {
		t.Errorf("TokenExpiry = %v, want %v", got.TokenExpiry, want.TokenExpiry)
	}
}

func TestFileOnDiskIsNotReadable(t *testing.T) {
	// The whole point of the store. If the token appears in the file, the
	// encryption is not doing its job.
	s, path := newStore(t)

	const token = "eyJhbGciOiJIUzI1NiJ9.SUPER_SECRET_DEVICE_TOKEN.sig"
	if err := s.Save(store.State{
		DeviceID: "dev_x", DeviceToken: token, UserID: "usr_x",
		PendingPrompts: []store.PendingPrompt{{PromptID: "p1", Text: "secret instruction"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	for _, secret := range []string{token, "dev_x", "usr_x", "secret instruction", "device_id"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("plaintext %q found in the state file", secret)
		}
	}
}

func TestTamperedFileIsRejected(t *testing.T) {
	// AES-GCM is authenticated, so a modified file must fail to decrypt rather
	// than yielding attacker-influenced plaintext.
	s, path := newStore(t)
	if err := s.Save(store.State{DeviceID: "dev_x", DeviceToken: "tok"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Flip a bit in the ciphertext body, past the nonce.
	raw[len(raw)-1] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	reopened := store.OpenWithKey(path, testKey)
	if _, err := reopened.Load(); !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt for a tampered file", err)
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	s, path := newStore(t)
	if err := s.Save(store.State{DeviceID: "dev_x", DeviceToken: "tok"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A different key stands in for a copied state file on another machine.
	var other store.StaticKey
	other[0] = 99

	reopened := store.OpenWithKey(path, other)
	if _, err := reopened.Load(); !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt when decrypting with the wrong key", err)
	}
}

func TestNonceIsUniquePerWrite(t *testing.T) {
	// Reusing a GCM nonce with the same key leaks the XOR of the plaintexts and
	// permits forgery. Identical state written twice must still produce different
	// ciphertext.
	s, path := newStore(t)
	state := store.State{DeviceID: "dev_x", DeviceToken: "tok"}

	if err := s.Save(state); err != nil {
		t.Fatalf("first save: %v", err)
	}
	first, _ := os.ReadFile(path)
	firstCopy := append([]byte(nil), first...)

	if err := s.Save(state); err != nil {
		t.Fatalf("second save: %v", err)
	}
	second, _ := os.ReadFile(path)

	if bytes.Equal(firstCopy, second) {
		t.Error("two writes of identical state produced identical ciphertext; the nonce is being reused")
	}
}

func TestUpdateIsAtomicUnderConcurrency(t *testing.T) {
	// Update exists so two goroutines changing different fields cannot clobber
	// each other, which a Load/mutate/Save sequence at call sites would.
	s, _ := newStore(t)
	if err := s.Save(store.State{DeviceID: "dev_x", DeviceToken: "tok"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Update(func(st *store.State) {
				st.PendingPrompts = append(st.PendingPrompts, store.PendingPrompt{
					PromptID: "p", Text: "queued",
				})
			})
		}(i)
	}
	wg.Wait()

	final := s.Current()
	if len(final.PendingPrompts) != n {
		t.Errorf("queued %d prompts concurrently, %d survived — writes were lost",
			n, len(final.PendingPrompts))
	}
	// The unrelated field must be untouched by the concurrent appends.
	if final.DeviceID != "dev_x" {
		t.Errorf("DeviceID = %q, want it preserved across updates", final.DeviceID)
	}
}

func TestSaveIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	s, path := newStore(t)
	for i := 0; i < 5; i++ {
		if err := s.Save(store.State{DeviceID: "dev_x", DeviceToken: "tok"}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temporary file %q was left behind", e.Name())
		}
	}
}

func TestResetClearsCredentials(t *testing.T) {
	s, path := newStore(t)
	if err := s.Save(store.State{DeviceID: "dev_x", DeviceToken: "tok"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	cleared := s.Current()
	if cleared.Registered() {
		t.Error("state should be empty after Reset")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the state file should be removed by Reset")
	}
	// Reset must be safe to call twice: it runs on the credential-rejected path,
	// which can fire more than once during a reconnect storm.
	if err := s.Reset(); err != nil {
		t.Errorf("second Reset should be a no-op, got %v", err)
	}
}

func TestTokenExpiringSoon(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		expiry time.Time
		want   bool
	}{
		{"unset expiry is never expiring", time.Time{}, false},
		{"90 days out", now.Add(90 * 24 * time.Hour), false},
		{"8 days out", now.Add(8 * 24 * time.Hour), false},
		// A week of margin: the agent may be offline for days, and discovering an
		// expired token when the user needs it is the worst possible moment.
		{"6 days out", now.Add(6 * 24 * time.Hour), true},
		{"already expired", now.Add(-time.Hour), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := store.State{TokenExpiry: tc.expiry}
			if got := s.TokenExpiringSoon(now); got != tc.want {
				t.Errorf("TokenExpiringSoon = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRegisteredRequiresBothIDAndToken(t *testing.T) {
	// A half-registered state would let the agent try to connect with no
	// credential and loop on rejection.
	cases := []struct {
		name  string
		state store.State
		want  bool
	}{
		{"both present", store.State{DeviceID: "dev_x", DeviceToken: "tok"}, true},
		{"token missing", store.State{DeviceID: "dev_x"}, false},
		{"id missing", store.State{DeviceToken: "tok"}, false},
		{"empty", store.State{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			if got := state.Registered(); got != tc.want {
				t.Errorf("Registered = %v, want %v", got, tc.want)
			}
		})
	}
}
