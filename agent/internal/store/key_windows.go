//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiKeyProvider protects the encryption key with the Windows Data Protection
// API.
//
// DPAPI encrypts with a key derived from the user's Windows credentials, so the
// protected blob is useless on another machine or under another account. That is
// the property worth having: a copied or backed-up state file cannot be decrypted
// elsewhere, which file permissions alone cannot achieve.
//
// The honest limitation, stated because it would be misleading not to: an attacker
// who already has code execution AS THIS USER can call CryptUnprotectData just as
// we do. This defends against file theft and offline analysis, not against local
// code execution. See agent/internal/store/doc.go.
type dpapiKeyProvider struct {
	mu      sync.Mutex
	path    string // where the protected key blob lives
	cached  []byte
	entropy []byte
}

// newPlatformKeyProvider returns the Windows key provider.
func newPlatformKeyProvider(stateDir string) KeyProvider {
	return &dpapiKeyProvider{
		path: filepath.Join(stateDir, "agent.key"),
		// Additional entropy binds the blob to Beuvian specifically, so another
		// application running as the same user cannot unprotect it by chance.
		entropy: []byte("beuvian-agent-state-v1"),
	}
}

func (p *dpapiKeyProvider) Describe() string {
	return "Windows DPAPI (bound to this user account)"
}

// Key returns the AES key, creating and protecting one on first use.
func (p *dpapiKeyProvider) Key() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached != nil {
		return p.cached, nil
	}

	blob, err := os.ReadFile(p.path)
	if err == nil {
		key, uerr := dpapiUnprotect(blob, p.entropy)
		if uerr != nil {
			// The blob exists but cannot be unprotected: a different Windows user,
			// a restored profile, or corruption. Recoverable only by re-registering,
			// so the error names that explicitly.
			return nil, fmt.Errorf(
				"cannot unprotect the local key (was this profile restored or copied from another machine?): %w", uerr)
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("stored key is %d bytes, want %d", len(key), keySize)
		}
		p.cached = key
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key file: %w", err)
	}

	// First run: mint a key and protect it.
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	protected, err := dpapiProtect(key, p.entropy)
	if err != nil {
		return nil, fmt.Errorf("protect key with DPAPI: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	if err := os.WriteFile(p.path, protected, 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}

	p.cached = key
	return key, nil
}

// dataBlob mirrors the Win32 DATA_BLOB structure.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

// bytes copies the blob's contents into Go memory and frees the Win32 allocation.
//
// The copy is required: the memory belongs to the Windows heap, and LocalFree
// invalidates it the moment we return.
func (b *dataBlob) bytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

func (b *dataBlob) free() {
	if b.pbData != nil {
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(b.pbData)))
		b.pbData = nil
	}
}

var (
	crypt32              = windows.NewLazySystemDLL("crypt32.dll")
	procCryptProtectData = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect   = crypt32.NewProc("CryptUnprotectData")
)

// cryptprotectUIForbidden stops Windows prompting the user with a dialog. The
// agent runs unattended, and a modal prompt on a headless start would hang it.
const cryptprotectUIForbidden = 0x1

func dpapiProtect(plaintext, entropy []byte) ([]byte, error) {
	in := newBlob(plaintext)
	ent := newBlob(entropy)
	var out dataBlob

	ret, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description
		uintptr(unsafe.Pointer(&ent)),
		0, // reserved
		0, // no prompt struct
		cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	defer out.free()
	return out.bytes(), nil
}

func dpapiUnprotect(ciphertext, entropy []byte) ([]byte, error) {
	in := newBlob(ciphertext)
	ent := newBlob(entropy)
	var out dataBlob

	ret, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description out
		uintptr(unsafe.Pointer(&ent)),
		0, // reserved
		0, // no prompt struct
		cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer out.free()
	return out.bytes(), nil
}
