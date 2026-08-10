// Package root for Beuvian's cross-cutting libraries.
//
// INVARIANT: this module has ZERO third-party dependencies and imports only the
// Go standard library. It is consumed by both `agent` and `backend`, and keeping
// it dependency-free means neither side inherits the other's dependency graph
// (the agent must not pull in Fiber; the backend must not pull in OS power
// syscalls). Adding a require here is an architectural decision — see
// docs/adr/0003-shared-module-is-protocol-only.md before doing so.
module github.com/bhuvan0808/beuviancode/shared

go 1.26
