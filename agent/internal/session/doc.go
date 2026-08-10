// Package session manages the lifecycle of a supervised coding session.
//
// This is the Desktop Agent's coordinator, and it is the only place that knows how
// the other pieces fit together. Everything else stays independent of it: the
// coding adapter does not know a WebSocket exists, the transport does not know what
// a coding agent is, and the power manager knows only about sleep. Concentrating
// the wiring here is what keeps those three testable in isolation.
//
// # Responsibilities (Phase 3)
//
//   - Own the protocol.AgentState machine and reject illegal transitions.
//   - Consume the adapter's output channel, batch lines, and emit LOG frames.
//   - Detect idleness and raise TASK_WAITING — the event that puts a
//     "Claude is waiting for you" notification on the user's phone.
//   - Detect completion and raise TASK_COMPLETE.
//   - Emit STATUS on every transition and at least once per status interval, so
//     the dashboard converges even after a lost frame.
//   - Hold the sleep inhibition for exactly as long as the state is Active(), and
//     release it on every exit path including a crash.
//   - Inject prompts from the backend, re-queueing when the adapter is not
//     accepting input rather than discarding them.
//   - Recover after a coding-agent crash without losing queued prompts.
//
// # Idle detection is a heuristic, and its cost is asymmetric
//
// Claude Code gives no machine-readable "I am waiting for you" signal, so idleness
// is inferred from output falling silent for session.idle_timeout. The two failure
// modes are not equally bad: a premature notification is merely annoying, while a
// missed one means the user believes work is still running when it stopped forty
// minutes ago. The detector should therefore lean toward notifying, and the
// dashboard should show the state clearly enough that a false positive is obvious.
//
// Populated in Phase 3.
package session
