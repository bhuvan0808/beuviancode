package app

import "github.com/bhuvan0808/beuviancode/backend/internal/adapter/auth"

// Small indirections so the application layer does not import the auth adapter at
// every call site.
//
// This is a pragmatic compromise, and worth naming as one. Strictly, `app` should
// depend only on `domain` and `port`, and these two helpers reach into `adapter`.
// The alternative would be a port interface with two methods that generate random
// identifiers, which adds a layer of indirection for no testability benefit:
// nothing about a random ID is worth substituting in a test.
//
// If a third such call appears, that is the signal this has stopped being a
// compromise and should become a proper port.

// newFamilyID mints a refresh-token family identifier.
func newFamilyID() string { return auth.NewFamilyID() }

// newState mints an OAuth state value for CSRF protection.
func newState() string { return auth.NewState() }
