package coding_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bhuvan0808/beuviancode/agent/internal/coding"
	blog "github.com/bhuvan0808/beuviancode/shared/log"
	"github.com/bhuvan0808/beuviancode/shared/protocol"
)

func TestRegisterPlaceholdersCoversEveryAdapterInTheSpec(t *testing.T) {
	r := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(r); err != nil {
		t.Fatalf("RegisterPlaceholders: %v", err)
	}
	// PROJECT.md names Claude plus four future adapters.
	for _, name := range []string{
		coding.AdapterClaude, coding.AdapterCodex,
		coding.AdapterGemini, coding.AdapterAider, coding.AdapterOpenHands,
	} {
		if !r.Has(name) {
			t.Errorf("adapter %q from PROJECT.md is not registered", name)
		}
	}
	if got := len(r.Names()); got != 5 {
		t.Errorf("registered %d adapters, want 5: %v", got, r.Names())
	}
}

func TestRegisterPlaceholdersIsNotIdempotent(t *testing.T) {
	// Calling it twice on one registry is a wiring bug and must be reported, not
	// silently tolerated.
	r := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(r); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := coding.RegisterPlaceholders(r); err == nil {
		t.Error("registering twice should fail rather than silently overwrite")
	}
}

func TestPlaceholderFailsLoudlyRatherThanSilently(t *testing.T) {
	// A placeholder that returned nil would present as a coding agent that
	// accepted the work and then did nothing — the worst possible behaviour,
	// because the user waits for a result that will never come.
	r := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(r); err != nil {
		t.Fatalf("RegisterPlaceholders: %v", err)
	}

	a, err := r.New(coding.AdapterAider, blog.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if err := a.Start(ctx, coding.StartOptions{WorkingDirectory: t.TempDir()}); !errors.Is(err, coding.ErrNotImplemented) {
		t.Errorf("Start err = %v, want ErrNotImplemented", err)
	}
	if err := a.SendPrompt(ctx, "do the thing"); !errors.Is(err, coding.ErrNotImplemented) {
		t.Errorf("SendPrompt err = %v, want ErrNotImplemented", err)
	}
}

func TestPlaceholderStopIsIdempotent(t *testing.T) {
	// Crash-recovery paths call Stop on an adapter that may never have started.
	r := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(r); err != nil {
		t.Fatalf("RegisterPlaceholders: %v", err)
	}
	a, err := r.New(coding.AdapterCodex, blog.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("Stop call %d returned %v, want nil", i, err)
		}
	}
}

func TestPlaceholderReadOutputTerminates(t *testing.T) {
	// A nil channel would block a consumer's range loop forever. A closed one
	// ends it immediately, which is what a non-running process should look like.
	r := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(r); err != nil {
		t.Fatalf("RegisterPlaceholders: %v", err)
	}
	a, err := r.New(coding.AdapterGemini, blog.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for range a.ReadOutput() { //nolint:revive // draining is the point
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadOutput did not terminate; a consumer would hang")
	}
}

func TestPlaceholderStatusAndAccessors(t *testing.T) {
	r := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(r); err != nil {
		t.Fatalf("RegisterPlaceholders: %v", err)
	}
	a, err := r.New(coding.AdapterOpenHands, blog.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if st := a.Status(); st.State != protocol.StateIdle {
		t.Errorf("State = %s, want idle", st.State)
	}
	// ExitCode's bool must be false while nothing has run, so "exited with 0" is
	// distinguishable from "never ran".
	if _, exited := a.ExitCode(); exited {
		t.Error("ExitCode should report not-exited for an adapter that never started")
	}
	// Accessors must return empty rather than inventing values.
	if a.CurrentTask() != "" || a.Repository() != "" || a.WorkingDirectory() != "" {
		t.Error("a non-running placeholder must not report a task, repository, or directory")
	}
}

func TestImplementedIsHonestAboutPhase1(t *testing.T) {
	// Phase 1 ships no working adapter. This test is the tripwire that Phase 3
	// must update deliberately rather than by accident.
	for _, name := range []string{
		coding.AdapterClaude, coding.AdapterCodex,
		coding.AdapterGemini, coding.AdapterAider, coding.AdapterOpenHands,
	} {
		if coding.Implemented(name) {
			t.Errorf("Implemented(%q) = true; update this test in the same commit "+
				"as the adapter implementation", name)
		}
	}
}

func TestClaudeDetectionCoversWindowsShim(t *testing.T) {
	// On Windows the npm installer produces claude.cmd, not claude.exe.
	// exec.LookPath only finds it if the name is tried explicitly, so omitting it
	// would make Beuvian report "not installed" on the very platform the MVP
	// targets. Detection must not error regardless of what is on this machine.
	r := coding.NewRegistry()
	if err := coding.RegisterPlaceholders(r); err != nil {
		t.Fatalf("RegisterPlaceholders: %v", err)
	}
	found, problems := r.DetectAll(context.Background())
	if len(problems) != 0 {
		t.Errorf("PATH detection should never fail hard, got: %v", problems)
	}
	// Whatever is or is not installed, an entry that IS found must be usable.
	for name, inst := range found {
		if inst.ExecutablePath == "" {
			t.Errorf("adapter %q reported as found with no executable path", name)
		}
	}
}
