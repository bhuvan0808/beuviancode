# Graph Report - .  (2026-08-05)

## Corpus Check
- 76 files · ~66,806 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 494 nodes · 934 edges · 35 communities (25 shown, 10 thin omitted)
- Extraction: 84% EXTRACTED · 16% INFERRED · 0% AMBIGUOUS · INFERRED: 145 edges (avg confidence: 0.8)
- Token cost: 0 input · 0 output

## Community Hubs (Navigation)
- Config Precedence Engine
- Coding Adapter Tests
- Backend Config Schema
- Coding Agent Placeholders
- Structured Logging
- Lifecycle Supervisor
- Protocol Payloads
- Retry & Backoff
- Cross-Platform Power
- Backend Entrypoint
- Agent Config Schema
- Agent State Machine
- Agent Config Tests
- Protocol Envelope
- Agent Entrypoint
- Bash Build Script
- Agent Config Validation
- PowerShell Build Script
- Go Module Roots
- Backend URL Validation
- Coding Config Validation
- Device Config Validation
- Offline Queue Validation
- Session Config Validation
- Rate Limit Validation
- Server Config Validation
- WebSocket Config Validation

## God Nodes (most connected - your core abstractions)
1. `Resolve()` - 22 edges
2. `Time()` - 21 edges
3. `NewRegistry()` - 19 edges
4. `Discard()` - 19 edges
5. `load()` - 18 edges
6. `Registry` - 15 edges
7. `run()` - 14 edges
8. `New()` - 14 edges
9. `New()` - 14 edges
10. `placeholder` - 13 edges

## Surprising Connections (you probably didn't know these)
- `run()` --calls--> `Format`  [INFERRED]
  agent/cmd/beuvian-agent/main.go → shared/log/logger.go
- `run()` --calls--> `Err()`  [INFERRED]
  agent/cmd/beuvian-agent/main.go → shared/log/logger.go
- `TestPlaceholderFailsLoudlyRatherThanSilently()` --calls--> `Discard()`  [INFERRED]
  agent/internal/coding/placeholder_test.go → shared/log/logger.go
- `TestPlaceholderStopIsIdempotent()` --calls--> `Discard()`  [INFERRED]
  agent/internal/coding/placeholder_test.go → shared/log/logger.go
- `TestPlaceholderReadOutputTerminates()` --calls--> `Discard()`  [INFERRED]
  agent/internal/coding/placeholder_test.go → shared/log/logger.go

## Import Cycles
- None detected.

## Communities (35 total, 10 thin omitted)

### Community 0 - "Config Precedence Engine"
Cohesion: 0.09
Nodes (38): DecodeFunc, field, Loader, Options, Result, sample, FlagSet, Value (+30 more)

### Community 1 - "Coding Adapter Tests"
Cohesion: 0.11
Nodes (32): RegisterPlaceholders(), T, TestClaudeDetectionCoversWindowsShim(), TestImplementedIsHonestAboutPhase1(), TestPlaceholderFailsLoudlyRatherThanSilently(), TestPlaceholderReadOutputTerminates(), TestPlaceholderStatusAndAccessors(), TestPlaceholderStopIsIdempotent() (+24 more)

### Community 2 - "Backend Config Schema"
Cohesion: 0.06
Nodes (29): Auth, Auth, Config, CORS, Database, Flags, Log, RateLimit (+21 more)

### Community 3 - "Coding Agent Placeholders"
Cohesion: 0.08
Nodes (13): detectOnPath(), Context, Logger, knownAdapters(), newPlaceholder(), readVersion(), Context, Installation (+5 more)

### Community 4 - "Structured Logging"
Cohesion: 0.15
Nodes (31): Attr, Buffer, Level, Config, contextKey, Format, CorrelationIDFrom(), Enrich() (+23 more)

### Community 5 - "Lifecycle Supervisor"
Cohesion: 0.15
Nodes (22): Component, Func, recorder, Supervisor, Context, Duration, Logger, New() (+14 more)

### Community 6 - "Protocol Payloads"
Cohesion: 0.12
Nodes (27): OutputLine, AckPayload, AuthPayload, DevicePresencePayload, HeartbeatPayload, LogPayload, LogStream, NotificationPayload (+19 more)

### Community 7 - "Retry & Backoff"
Cohesion: 0.14
Nodes (22): Backoff, Permanent, Policy, Uint64(), DefaultPolicy(), Do(), Fatal(), Context (+14 more)

### Community 8 - "Cross-Platform Power"
Cohesion: 0.09
Nodes (16): Logger, newPlatformManager(), Duration, Logger, Mutex, Logger, newPlatformManager(), New() (+8 more)

### Community 9 - "Backend Entrypoint"
Cohesion: 0.18
Nodes (24): Config, Logger, main(), orNone(), run(), warnOnDevelopmentShortcuts(), Load(), Config (+16 more)

### Community 10 - "Agent Config Schema"
Cohesion: 0.12
Nodes (20): configDir(), DefaultStatePath(), Backend, Coding, Config, Device, Flags, Log (+12 more)

### Community 11 - "Agent State Machine"
Cohesion: 0.10
Nodes (10): Duration, Status, AgentState, ErrorCode, ErrorPayload, StatusPayload, T, TestAgentStateActiveDrivesPowerManagement() (+2 more)

### Community 12 - "Agent Config Tests"
Cohesion: 0.24
Nodes (20): Config, T, load(), TestAgentUsesItsOwnEnvPrefix(), TestAutoStartRequiresAWorkingDirectory(), TestBackendURLSchemeIsEnforced(), TestDescribeRedactsTheDeviceToken(), TestDeviceIDShapeIsChecked() (+12 more)

### Community 13 - "Protocol Envelope"
Cohesion: 0.16
Nodes (14): Envelope, MessageType, RawMessage, Decode(), Duration, T, NewEnvelope(), T (+6 more)

### Community 14 - "Agent Entrypoint"
Cohesion: 0.24
Nodes (13): Logger, Writer, main(), newPowerComponent(), openLogWriter(), orNone(), run(), runDetect() (+5 more)

### Community 15 - "Bash Build Script"
Cohesion: 0.40
Nodes (4): BUILT, CGO_ENABLED, GOWORK, build-agent.sh script

### Community 18 - "Go Module Roots"
Cohesion: 0.67
Nodes (3): github.com/bhuvan0808/beuviancode/agent, github.com/bhuvan0808/beuviancode/backend, github.com/bhuvan0808/beuviancode/shared

## Knowledge Gaps
- **30 isolated node(s):** `github.com/bhuvan0808/beuviancode/agent`, `Device`, `Coding`, `Queue`, `Log` (+25 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **10 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `run()` connect `Agent Entrypoint` to `Coding Adapter Tests`, `Agent Config Schema`, `Structured Logging`?**
  _High betweenness centrality (0.393) - this node is a cross-community bridge._
- **Why does `Load()` connect `Agent Config Schema` to `Config Precedence Engine`, `Agent Config Tests`, `Agent Entrypoint`?**
  _High betweenness centrality (0.271) - this node is a cross-community bridge._
- **Why does `Time()` connect `Protocol Payloads` to `Agent State Machine`, `Cross-Platform Power`, `Coding Agent Placeholders`, `Protocol Envelope`?**
  _High betweenness centrality (0.260) - this node is a cross-community bridge._
- **Are the 17 inferred relationships involving `Resolve()` (e.g. with `Load()` and `Load()`) actually correct?**
  _`Resolve()` has 17 INFERRED edges - model-reasoned connections that need verification._
- **Are the 2 inferred relationships involving `Time()` (e.g. with `TestTimeRecoversEncodedTimestamp()` and `TestWithPrefixAndValidate()`) actually correct?**
  _`Time()` has 2 INFERRED edges - model-reasoned connections that need verification._
- **Are the 17 inferred relationships involving `NewRegistry()` (e.g. with `run()` and `TestClaudeDetectionCoversWindowsShim()`) actually correct?**
  _`NewRegistry()` has 17 INFERRED edges - model-reasoned connections that need verification._
- **Are the 14 inferred relationships involving `Discard()` (e.g. with `TestPlaceholderFailsLoudlyRatherThanSilently()` and `TestPlaceholderReadOutputTerminates()`) actually correct?**
  _`Discard()` has 14 INFERRED edges - model-reasoned connections that need verification._