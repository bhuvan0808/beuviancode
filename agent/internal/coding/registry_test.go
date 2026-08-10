package coding_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/bhuvan0808/beuviancode/agent/internal/coding"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

// stubAdapter is a minimal Adapter, used to prove the interface is satisfiable
// by something that is not Claude. If this stops compiling, the abstraction has
// grown Claude-specific and the pluggability PROJECT.md requires is a fiction.
type stubAdapter struct{ name string }

func (s *stubAdapter) Name() string                                     { return s.name }
func (s *stubAdapter) Start(context.Context, coding.StartOptions) error { return nil }
func (s *stubAdapter) Stop(context.Context) error                       { return nil }
func (s *stubAdapter) Status() coding.Status                            { return coding.Status{State: protocol.StateIdle} }
func (s *stubAdapter) SendPrompt(context.Context, string) error         { return nil }
func (s *stubAdapter) ReadOutput() <-chan coding.OutputLine {
	ch := make(chan coding.OutputLine)
	close(ch)
	return ch
}
func (s *stubAdapter) CurrentTask() string      { return "" }
func (s *stubAdapter) Repository() string       { return "" }
func (s *stubAdapter) WorkingDirectory() string { return "" }
func (s *stubAdapter) ExitCode() (int, bool)    { return 0, false }

// Compile-time assertion that the stub satisfies the interface.
var _ coding.Adapter = (*stubAdapter)(nil)

func stubFactory(name string) coding.Factory {
	return func(*slog.Logger) coding.Adapter { return &stubAdapter{name: name} }
}

type stubDetector struct {
	name string
	inst coding.Installation
	err  error
}

func (d stubDetector) Name() string { return d.name }
func (d stubDetector) Detect(context.Context) (coding.Installation, error) {
	return d.inst, d.err
}

func TestRegisterAndConstruct(t *testing.T) {
	r := coding.NewRegistry()
	if err := r.Register("stub", stubFactory("stub"), nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !r.Has("stub") {
		t.Error("Has should report a registered adapter")
	}

	a, err := r.New("stub", blog.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Name() != "stub" {
		t.Errorf("Name = %q, want stub", a.Name())
	}
}

func TestUnknownAdapterIsRejected(t *testing.T) {
	r := coding.NewRegistry()
	_, err := r.New("nonexistent", blog.Discard())
	if !errors.Is(err, coding.ErrUnsupportedAdapter) {
		t.Errorf("err = %v, want ErrUnsupportedAdapter", err)
	}
}

func TestDuplicateRegistrationIsRejected(t *testing.T) {
	// Returning an error rather than panicking: a name conflict must not take
	// down a user's agent over something recoverable.
	r := coding.NewRegistry()
	if err := r.Register("stub", stubFactory("stub"), nil); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register("stub", stubFactory("stub"), nil); err == nil {
		t.Error("a duplicate registration should be rejected")
	}
}

func TestRegisterRejectsInvalidInput(t *testing.T) {
	r := coding.NewRegistry()
	if err := r.Register("", stubFactory("x"), nil); err == nil {
		t.Error("an empty adapter name should be rejected")
	}
	if err := r.Register("nilfactory", nil, nil); err == nil {
		t.Error("a nil factory should be rejected")
	}
}

func TestNamesAreSorted(t *testing.T) {
	// Sorted output keeps logs, help text, and the AUTH capability list stable
	// across runs; map iteration order would make them churn.
	r := coding.NewRegistry()
	for _, n := range []string{"zeta", "alpha", "mike"} {
		if err := r.Register(n, stubFactory(n), nil); err != nil {
			t.Fatalf("Register %s: %v", n, err)
		}
	}
	got := r.Names()
	want := []string{"alpha", "mike", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names = %v, want %v", got, want)
		}
	}
}

func TestDetectAllSeparatesAbsenceFromFailure(t *testing.T) {
	// The key behaviour: "not installed" is an ordinary outcome, while a broken
	// detector is a problem. Collapsing the two would make a single misbehaving
	// tool look like an empty machine.
	r := coding.NewRegistry()
	broken := errors.New("permission denied reading PATH")

	mustRegister(t, r, "present", stubDetector{
		name: "present",
		inst: coding.Installation{ExecutablePath: "/usr/bin/present", Version: "1.2.3"},
	})
	mustRegister(t, r, "absent", stubDetector{name: "absent", err: coding.ErrNotInstalled})
	mustRegister(t, r, "broken", stubDetector{name: "broken", err: broken})

	found, problems := r.DetectAll(context.Background())

	if _, ok := found["present"]; !ok {
		t.Error("an installed adapter should appear in found")
	}
	if found["present"].Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", found["present"].Version)
	}
	if _, ok := found["absent"]; ok {
		t.Error("an uninstalled adapter must not appear in found")
	}
	if _, ok := problems["absent"]; ok {
		t.Error("ErrNotInstalled is an ordinary outcome, not a problem")
	}
	if !errors.Is(problems["broken"], broken) {
		t.Errorf("a detector failure should be reported, got %v", problems["broken"])
	}
	// One broken detector must not hide the working ones.
	if len(found) != 1 {
		t.Errorf("found = %v, want exactly the installed adapter", found)
	}
}

func TestDetectAllRespectsWrappedNotInstalled(t *testing.T) {
	// Detectors wrap ErrNotInstalled with context; errors.Is must still classify
	// it as absence rather than as a failure.
	r := coding.NewRegistry()
	mustRegister(t, r, "wrapped", stubDetector{
		name: "wrapped",
		err:  errors.New("looked everywhere: " + coding.ErrNotInstalled.Error()),
	})
	// A non-wrapping error of similar text must be treated as a problem.
	_, problems := r.DetectAll(context.Background())
	if len(problems) != 1 {
		t.Errorf("a plain error must not be mistaken for ErrNotInstalled: %v", problems)
	}

	r2 := coding.NewRegistry()
	mustRegister(t, r2, "properly", stubDetector{
		name: "properly",
		err:  errWrap(coding.ErrNotInstalled),
	})
	found2, problems2 := r2.DetectAll(context.Background())
	if len(problems2) != 0 {
		t.Errorf("a wrapped ErrNotInstalled should be absence, not a problem: %v", problems2)
	}
	if len(found2) != 0 {
		t.Errorf("found = %v, want empty", found2)
	}
}

func TestCapabilitiesReflectsWhatIsInstalled(t *testing.T) {
	// The backend uses capabilities to avoid dispatching a prompt to a device
	// that cannot service it, so this must report installed tools, not compiled-in
	// support.
	r := coding.NewRegistry()
	mustRegister(t, r, "here", stubDetector{name: "here", inst: coding.Installation{ExecutablePath: "/bin/here"}})
	mustRegister(t, r, "gone", stubDetector{name: "gone", err: coding.ErrNotInstalled})

	caps := r.Capabilities(context.Background())
	if len(caps) != 1 || caps[0] != "here" {
		t.Errorf("Capabilities = %v, want [here]", caps)
	}
	// Both are registered, so Names is wider than Capabilities. That gap is the
	// whole point.
	if len(r.Names()) != 2 {
		t.Errorf("Names = %v, want both registered adapters", r.Names())
	}
}

func TestDetectAllHonoursContextCancellation(t *testing.T) {
	r := coding.NewRegistry()
	mustRegister(t, r, "slow", stubDetector{name: "slow", inst: coding.Installation{ExecutablePath: "/bin/slow"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, problems := r.DetectAll(ctx)
	if len(problems) == 0 {
		t.Error("a cancelled context should be reported rather than silently ignored")
	}
}

func mustRegister(t *testing.T, r *coding.Registry, name string, d coding.Detector) {
	t.Helper()
	if err := r.Register(name, stubFactory(name), d); err != nil {
		t.Fatalf("Register %s: %v", name, err)
	}
}

func errWrap(err error) error { return wrapped{err} }

type wrapped struct{ err error }

func (w wrapped) Error() string { return "detect: " + w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }
