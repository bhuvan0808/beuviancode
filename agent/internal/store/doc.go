// Package store persists the agent's local state, encrypted at rest.
//
// # What is stored
//
//   - The device ID assigned at registration.
//   - The device access token and its refresh token.
//   - The offline prompt queue, so a prompt sent from a phone survives an agent
//     restart rather than vanishing.
//   - The last known session context, for crash recovery.
//
// # Why encryption, when the file is already behind OS permissions
//
// PROJECT.md requires "Encrypted Local Config", and the requirement is
// well-founded. The file holds a long-lived credential that grants control over a
// developer's machine. File permissions alone do not protect it against a synced
// backup, a cloud-mirrored home directory, a shared build machine, or a laptop
// disk read offline. Permissions plus encryption is the defensible combination.
//
// # Key management (Phase 3)
//
// The honest constraint: an unattended agent must start with no user present to
// type a passphrase, so the key has to be recoverable by the process itself. That
// makes this obfuscation-plus-OS-protection rather than true secrecy from a
// determined local attacker, and pretending otherwise would be worse than saying
// it plainly.
//
// The plan is therefore to delegate key storage to the platform's own secret
// store, which is designed for exactly this problem:
//
//	Windows  DPAPI (CryptProtectData), scoped to the user account
//	macOS    Keychain Services
//	Linux    Secret Service via D-Bus (GNOME Keyring, KWallet)
//
// Each binds the key to the OS user, so a copied file is useless on another
// machine. Where no secret store exists — a headless Linux box — the fallback is
// a key file at 0600 alongside the state, with the reduced protection logged
// rather than hidden.
//
// # Durability
//
// Writes are atomic: serialise, write to a temporary file in the same directory,
// fsync, then rename over the target. A partial write during a crash or power loss
// would otherwise leave an unparseable state file, and the recovery from that is
// re-registering the device and losing the queued prompts — the exact data this
// package exists to protect.
//
// Populated in Phase 3.
package store
