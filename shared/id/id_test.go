package id_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/shared/id"
)

func TestNewShapeAndAlphabet(t *testing.T) {
	got := id.New()
	if len(got) != id.Length {
		t.Fatalf("len = %d, want %d (%q)", len(got), id.Length, got)
	}
	// Crockford base32 excludes I, L, O, U so IDs cannot be misread aloud.
	for _, forbidden := range []string{"I", "L", "O", "U"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("ID %q contains ambiguous character %q", got, forbidden)
		}
	}
	if err := id.Validate(got); err != nil {
		t.Errorf("freshly generated ID failed validation: %v", err)
	}
}

func TestNewIsUnique(t *testing.T) {
	const n = 20000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		v := id.New()
		if _, dup := seen[v]; dup {
			t.Fatalf("collision after %d ids: %s", i, v)
		}
		seen[v] = struct{}{}
	}
}

func TestNewIsLexicographicallySortableByTime(t *testing.T) {
	// The whole reason for choosing ULID over UUIDv4 is index locality from
	// time ordering. If this breaks, the justification is gone.
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	earlier := id.NewAt(base)
	later := id.NewAt(base.Add(time.Second))
	if earlier >= later {
		t.Errorf("expected %q < %q for ids one second apart", earlier, later)
	}
}

func TestTimeRecoversEncodedTimestamp(t *testing.T) {
	// Truncate to milliseconds: that is the encoded resolution.
	want := time.Date(2026, 8, 5, 12, 34, 56, 789000000, time.UTC)
	got, err := id.Time(id.NewAt(want))
	if err != nil {
		t.Fatalf("Time: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("Time = %v, want %v", got, want)
	}
}

func TestWithPrefixAndValidate(t *testing.T) {
	v := id.WithPrefix(id.PrefixDevice)
	if !strings.HasPrefix(v, "dev_") {
		t.Errorf("WithPrefix = %q, want dev_ prefix", v)
	}
	if err := id.Validate(v); err != nil {
		t.Errorf("prefixed ID failed validation: %v", err)
	}
	if _, err := id.Time(v); err != nil {
		t.Errorf("Time should work on prefixed IDs: %v", err)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	bad := []struct{ name, in string }{
		{"empty", ""},
		{"too short", "01J9Z3K7QF"},
		{"too long", "01J9Z3K7QF8XKM2N4P6R8T0VWYZZZ"},
		{"ambiguous letter I", "01J9Z3K7QF8XKM2N4P6R8T0VWI"},
		{"lowercase", "01j9z3k7qf8xkm2n4p6r8t0vwy"},
		{"trailing underscore", "dev_"},
		{"leading underscore", "_01J9Z3K7QF8XKM2N4P6R8T0VWY"},
		{"sql injection attempt", "dev_'; DROP TABLE devices;--"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := id.Validate(tc.in); err == nil {
				t.Errorf("Validate(%q) = nil, want an error", tc.in)
			}
		})
	}
}

func TestNonceIsUnpredictableAndDistinct(t *testing.T) {
	const n = 5000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		v := id.Nonce()
		if _, dup := seen[v]; dup {
			t.Fatalf("nonce collision at %d: %s", i, v)
		}
		seen[v] = struct{}{}
	}
	// Unlike New(), a nonce must carry no recoverable timestamp prefix; two
	// nonces minted together must not share a leading run.
	a, b := id.Nonce(), id.Nonce()
	if a[:6] == b[:6] {
		t.Errorf("nonces share a leading prefix (%q, %q); they must be fully random", a, b)
	}
}
