# epochd — implementation context (current phase: 40)

This file is a dense reference for an agent or developer continuing the project. It
captures the state of the codebase after phases 0–40, the exact APIs that exist, every
non-obvious decision that was made and why, and discovered gotchas. Phase 38
(`Handle.EffectiveTime`, `Handle.PID`, `Session.Close`, `Handle.IsAlive`) is planned but
not yet implemented. Phases 39–40 are complete. See `FUTURE.md` for longer-horizon
improvements.

---

## Module and build facts

- **Module**: `github.com/bkaznowski/epochd`
- **Go version**: `go 1.26.4` (in go.mod)
- **Direct dependencies**:
  - `golang.org/x/sys v0.46.0` — ptrace, `process_vm_readv/writev`, `MAP_FIXED_NOREPLACE`
  - `google.golang.org/grpc v1.81.1` — controller→agent gRPC transport
  - `google.golang.org/protobuf v1.36.x` — generated protobuf types
  - `k8s.io/api v0.32.5`, `k8s.io/apimachinery v0.32.5`, `k8s.io/client-go v0.32.5` — pod listing and informers
- **Build constraint on most files**: `//go:build linux` — the vDSO hook is Linux x86-64 only. `pkg/api`, `pkg/sdk`, `pkg/agentclient`, `pkg/agentpb`, and `test/targets/clockprinter` have no build tag (cross-platform).
- **No CGo anywhere.** The trampoline binary is embedded bytes; no native compilation at runtime.
- **Cross-compile from Windows/Mac**: `GOOS=linux GOARCH=amd64 go build ./...` works fine; the `//go:build linux` tag is for runtime, not the host.
- **golangci-lint**: configured in `.golangci.yml`; CI uses `install-mode: goinstall` so the linter is built with the project's own Go version (avoids version mismatch rejections from older pre-built linter binaries).

---

## Package inventory

### `pkg/vdso`

**File**: `pkg/vdso/vdso.go`

```go
type VDSOInfo struct {
    Start, End       uintptr
    ClockGettimeAddr uintptr  // absolute address in target process
}

func Locate(pid int) (*VDSOInfo, error)
```

**What it does**: parses `/proc/<pid>/maps` for `[vdso]`, reads those bytes via
`/proc/<pid>/mem` (requires ptrace relationship or same process), parses with
`debug/elf.NewFile`, resolves `clock_gettime` (falling back to `__vdso_clock_gettime`),
sanity-checks the resolved address falls within `[Start, End)`.

**Caller must**: be ptrace-attached to `pid` before calling (or be reading their own
process). `inject.inject` calls `vdso.Locate` before `Attach`, which works because
`/proc/<pid>/mem` is readable once you have `CAP_SYS_PTRACE`, even before attaching.

**Test**: `pkg/vdso/locate_test.go` — calls `Locate(os.Getpid())`, re-reads ELF
independently, asserts symbol type is `STT_FUNC`, size > 0, address matches, falls in
executable PT_LOAD segment, non-zero bytes at offset.

---

### `pkg/procmem`

**File**: `pkg/procmem/procmem.go`

```go
type Tracer struct { /* channel to a pinned-OS-thread goroutine */ }

func NewTracer() *Tracer

// For children spawned with SysProcAttr{Ptrace: true} — no PTRACE_ATTACH, waits for SIGTRAP.
func (t *Tracer) FollowChild(pid int) error

// For attaching to an already-running process — requires CAP_SYS_PTRACE + ptrace_scope ≤ 1.
func (t *Tracer) Attach(pid int) error

func (t *Tracer) Detach() error
func (t *Tracer) GetRegs() (*unix.PtraceRegs, error)
func (t *Tracer) SetRegs(r *unix.PtraceRegs) error
func (t *Tracer) SingleStep() error
func (t *Tracer) Cont(sig int) error                        // sig=0 means no signal
func (t *Tracer) Wait() (unix.WaitStatus, error)

// Writes to read-only pages (e.g. vDSO). Requires active ptrace attachment.
func (t *Tracer) PokeText(addr uintptr, buf []byte) error

// Bulk IO — does NOT need ptrace stop; does need ptrace relationship or CAP_SYS_PTRACE.
func ReadMem(pid int, addr uintptr, buf []byte) (int, error)   // process_vm_readv
func WriteMem(pid int, addr uintptr, buf []byte) (int, error)  // process_vm_writev

// Added in phases 39–40 (child tracking / exec-survivor injection):

// SetTracee changes which PID Tracer methods operate on. Used when temporarily
// re-targeting to inject into an exec'd child; caller must restore the original PID.
func (t *Tracer) SetTracee(pid int)

// SetOptions sets PTRACE_O_* options on the current tracee (must be ptrace-stopped).
func (t *Tracer) SetOptions(opts int) error

// SetOptionsPID sets PTRACE_O_* options on an arbitrary ptrace-stopped PID without
// changing t.pid. Used to arm forked children with their own TRACEFORK/TRACEEXEC.
func (t *Tracer) SetOptionsPID(pid, opts int) error

// WaitAnyNonBlocking checks for a stop from any traced child without blocking.
// Returns pid=0 when no events are pending; ECHILD when no traced children remain.
func (t *Tracer) WaitAnyNonBlocking() (int, unix.WaitStatus, error)

// GetEventMsgPID returns the ptrace event message from an arbitrary stopped PID.
// After PTRACE_EVENT_FORK/VFORK it returns the child PID; after PTRACE_EVENT_EXEC
// it returns the pre-exec PID.
func (t *Tracer) GetEventMsgPID(pid int) (uint, error)

// ContPID resumes an arbitrary ptrace-stopped PID. sig=0 delivers no signal.
func (t *Tracer) ContPID(pid, sig int) error

// InterruptDetach sends PTRACE_INTERRUPT to the current tracee, waits for the
// resulting stop, then detaches. Safe to call when the tracee is running.
func (t *Tracer) InterruptDetach() error

// DetachAll interrupts and detaches from each PID in the list. ESRCH errors
// (process already gone) are silently ignored.
func (t *Tracer) DetachAll(pids []int) error
```

**Critical design**: all ptrace syscalls must come from the same OS thread that issued
`PTRACE_ATTACH`. Go's scheduler moves goroutines freely, which breaks this. The `Tracer`
spawns one goroutine at construction that immediately calls `runtime.LockOSThread()` and
never unlocks. All exported methods send closures over a channel to that goroutine and
block for completion. Do not call any `unix.Ptrace*` function outside the Tracer's
dispatch loop.

**`FollowChild` vs `Attach`**: `Attach` (which calls `PTRACE_ATTACH`) fails in Docker
even with `--cap-add SYS_PTRACE --security-opt seccomp=unconfined` because of Yama
`ptrace_scope=1` enforced by Docker's user namespace setup. `FollowChild` works everywhere
because the child called `PTRACE_TRACEME` itself. All tests use the `FollowChild` path.
Production code (`faketimectl`, the phase-7 agent) uses `Attach` — this is fine because
in Kubernetes the agent runs in a privileged pod with `hostPID: true` where ptrace_scope
can be set or overridden via seccomp.

---

### `pkg/trampoline`

**Files**: `pkg/trampoline/trampoline.go`, `trampoline.asm`, `trampoline.bin`

```go
//go:embed trampoline.bin
var Payload []byte          // 118 bytes total

const StateOffset = 86      // byte offset of state struct within Payload
const StateSize   = 32      // 8+8+8+4+4

// Field offsets (absolute, from Payload[0]):
const FieldOffsetSec  = 86   // int64
const FieldOffsetNsec = 94   // int64
const FieldEnabledMask = 102 // uint64, bit 0 = CLOCK_REALTIME
const FieldGeneration  = 110 // uint32

func EncodeState(offsetSec, offsetNsec int64, mask uint64, generation uint32) []byte
func DecodeState(b []byte) (offsetSec, offsetNsec int64, mask uint64, generation uint32, err error)
```

**State struct layout** (at `Payload[StateOffset:]` and at `h.StateAddr` in the target):
```
+0   int64  offsetSec     — added to tp->tv_sec
+8   int64  offsetNsec    — added to tp->tv_nsec (normalised to [0,1e9) by trampoline)
+16  uint64 enabledMask   — bit 0 = CLOCK_REALTIME enabled
+24  uint32 generation    — bumped on each SetTime call, for observability
+28  uint32 _pad
```

**Trampoline behaviour**: on every `clock_gettime` call — (1) pushes rdi/rsi, issues raw
`syscall 228` to get the real time, restores rdi/rsi, (2) if `clk_id != CLOCK_REALTIME`
returns immediately, (3) loads `offsetSec`/`offsetNsec` via `lea r11, [rel state]`
(RIP-relative, position-independent), (4) adds them to `tp->tv_sec`/`tp->tv_nsec`, (5)
normalises `tv_nsec` into `[0, 1e9)` with a single add or sub, (6) returns 0.

**Only `CLOCK_REALTIME` (id=0) is intercepted.** All other clock IDs pass through the
real syscall result untouched. Go's `time.Now()` uses `CLOCK_REALTIME`.

**`StateOffset = 86` is a hardcoded constant.** `TestStateOffsetRegression` asserts
`StateOffset == len(Payload) - StateSize` and will fail loudly if the assembly is edited
and the binary changes size without updating the constant. Check this test first if you
ever touch the assembly.

**To reassemble**: `nasm -f bin pkg/trampoline/trampoline.asm -o pkg/trampoline/trampoline.bin`
Then update `StateOffset` if `wc -c trampoline.bin` changed.

---

### `pkg/inject`

**Files**: `pkg/inject/inject.go`, `inject_test.go`, `roundtrip_test.go`

#### Public API

```go
type Handle struct {
    PID       int
    StateAddr uintptr  // address of state struct in target process
    // unexported: origBytes [5]byte, gen uint32
}

// PTRACE_ATTACH path (requires CAP_SYS_PTRACE + ptrace_scope ≤ 1):
func InjectAtTime(pid int, target time.Time) (*Handle, error)   // advancing mode
func InjectFrozen(pid int, target time.Time) (*Handle, error)   // frozen mode

// FollowChild path (no elevated perms; child must have SysProcAttr{Ptrace: true}):
func InjectAtTimeFollowChild(pid int, target time.Time) (*Handle, error)
func InjectFrozenFollowChild(pid int, target time.Time) (*Handle, error)

// FollowChild path, keeping the Tracer attached for ChildTracker use (phase 39):
func InjectAtTimeFollowChildKeepTracer(pid int, target time.Time) (*Handle, *procmem.Tracer, error)
func InjectFrozenFollowChildKeepTracer(pid int, target time.Time) (*Handle, *procmem.Tracer, error)

// ChildHandle creates a Handle for a forked child that shares the parent's
// trampoline (same StateAddr, different PID). No injection needed — fork copies
// the address space including the trampoline page and vDSO patch.
func ChildHandle(parent *Handle, childPID int) *Handle

// Re-injection after exec() (phase 40). Temporarily retargets tr to pid, performs
// a full fresh injection (vDSO locate → mmap → patch), then restores tr to parentPID.
func ReInjectAtTimeAfterExec(tr *procmem.Tracer, parentPID, pid int, target time.Time) (*Handle, error)
func ReInjectFrozenAfterExec(tr *procmem.Tracer, parentPID, pid int, target time.Time) (*Handle, error)

// Live updates — single process_vm_writev, no ptrace stop required.
func (h *Handle) SetTime(target time.Time) error    // advancing mode
func (h *Handle) Freeze(target time.Time) error     // frozen mode
func (h *Handle) Generation() uint32                // observability
```

Clock modes:
- **Advancing** (`MaskEnabled = 1`): trampoline adds `(offsetSec, offsetNsec)` to real time.
- **Frozen** (`MaskFrozen = 3`): `offsetSec`/`offsetNsec` store the absolute frozen timestamp;
  the trampoline ignores real time and returns that value directly.

#### Internal functions (used in tests)

```go
// Core injection — tr must already be attached.
func injectWithTracer(tr *procmem.Tracer, pid int, cgtAddr uintptr, sec, nsec int64, mask uint64) (*Handle, error)

// Writes state struct with process_vm_writev, increments h.gen.
func (h *Handle) writeState(sec, nsec int64, mask uint64) error

// Converts absolute target time to (sec, nsec) offset from base.
// nsec is in (-1e9, 1e9); trampoline normalises to [0, 1e9).
func diffSecNsec(target, base time.Time) (sec, nsec int64)

// Scans /proc/<pid>/maps for first unmapped page-aligned gap within ±2 GB of near.
func findNearbyGap(pid int, near uintptr) (uintptr, error)

// Injects mmap syscall into tracee via patching patchAddr with syscall+int3.
func remoteMmap(tr *procmem.Tracer, pid int, patchAddr, fixedAddr uintptr) (uintptr, error)
```

#### Injection sequence (what `injectWithTracer` does)

1. `findNearbyGap(pid, cgtAddr)` — find a free page within ±2 GB of vDSO entry
2. `remoteMmap(tr, pid, cgtAddr, fixedAddr)` — make target call `mmap` with `MAP_FIXED_NOREPLACE`
3. Build payload: copy `trampoline.Payload`, overwrite state struct with encoded initial offsets
4. `procmem.WriteMem(pid, newPage, payload)` — write trampoline (new page is rwx, no ptrace needed)
5. Compute `disp = int64(newPage) - int64(cgtAddr+5)`, assert fits in int32
6. `procmem.ReadMem(pid, cgtAddr, orig[:5])` — save for future uninstall
7. `tr.PokeText(cgtAddr, [0xE9, disp_le32])` — patch vDSO entry with JMP rel32
8. Return `Handle{PID: pid, StateAddr: newPage + StateOffset, origBytes: orig}`

#### `findNearbyGap` — the critical fix for Docker

Plain `mmap(hint, ...)` is ignored by the kernel when address space near the vDSO is
saturated (observed: vDSO at `0x7fff...`, allocation at `0x78c9...` — 7.9 TB apart, way
beyond JMP rel32 reach). The fix: scan `/proc/<pid>/maps` (stable because tracee is
ptrace-stopped), find a gap in the ±2 GB window, pass it with `MAP_FIXED_NOREPLACE`.
Observed working allocation: vDSO at `0x7ffe65...a40`, trampoline at `0x7ffde5...000`,
displacement = `-2147482181` (fits in int32 with ~6 bytes to spare).

#### Test helper processes

Tests that need a live target process spawn the test binary itself in a special mode via
an environment variable:

```go
const helperEnv = "EPOCHD_INJECT_HELPER"
// env=1: TestInjectHelperBlock — blocks indefinitely (select{})
// env=2: TestInjectHelperClock — prints time.Now().Format(RFC3339Nano) every 100ms to stdout
```

Pattern used by all inject tests:
```go
cmd := exec.Command(exe, "-test.run=TestInjectHelper...", "-test.v")
cmd.Env = append(os.Environ(), helperEnv+"=N")
cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
cmd.Start()
tr := procmem.NewTracer()
tr.FollowChild(cmd.Process.Pid)
// ... test body ...
t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
```

---

### `cmd/faketimectl`

Two groups of subcommands. All controller subcommands accept `--url` or `EPOCHD_URL`:

```
# Controller subcommands (talk to a running epochd controller):
faketimectl create  --namespace=NS --selector=SEL --time=RFC3339 [--ttl=DUR] [--freeze]
faketimectl list
faketimectl get     <id>
faketimectl update  <id> --time=RFC3339 [--freeze]
faketimectl advance <id> --by=DURATION       # shift by a Go duration, e.g. "24h"
faketimectl delete  <id>
faketimectl status  <id>
faketimectl resolve --namespace=NS --selector=SEL

# Local injection subcommands (Linux only, CAP_SYS_PTRACE / ptrace_scope ≤ 1):
faketimectl inject  --pid=PID --time=RFC3339 [--freeze]
faketimectl reset   --pid=PID
```

`inject` calls `inject.InjectAtTime` or `inject.InjectFrozen` (re-injects on each call,
which is safe). Uses `PTRACE_ATTACH`.

---

### `test/targets/clockprinter`

Prints `time.Now().Format(time.RFC3339)` once per second. No build tag. Used as a manual
injection target with `faketimectl`. The inject tests use an in-binary helper instead
(env=2 above) to avoid needing a pre-built binary.

---

### Packages added in phases 7–37

| Package | Phase | Summary |
|---------|-------|---------|
| `pkg/agentpb` | 7 | Generated gRPC types from `proto/agent/v1/agent.proto` |
| `pkg/agentclient` | 7 | gRPC connection pool (`Pool`) satisfying `AgentPool` interface |
| `pkg/k8sresolve` | 7 | Container ID → PID resolution by scanning `/proc/*/cgroup` |
| `pkg/api` | 8 | Shared HTTP request/response types (no build tag) |
| `pkg/sdk` | 10 | Go client library; `Client`, `Timeshift`, `WithTimeT`, `ListTimeshifts`, `AdvanceTimeshift`, freeze helpers |
| `pkg/faketime` | 25 | Non-Kubernetes injection: `Start`, `StartFrozen`, `Attach`, `AttachFrozen`, `Handle`, `Session` |
| `pkg/faketime` (39–40) | 39–40 | `StartWithTracking`, `StartFrozenWithTracking`, `ChildTracker` (fork + exec-survivor auto-injection) |
| `cmd/agent` | 7 | gRPC daemon: CRI→PID, inject, SetTime, Reset, handle map |
| `cmd/controller` | 8 | HTTP+JSON API: timeshifts CRUD, TTL sweeper, pod watcher, advance-by-duration |
| `deploy/` | 9 | `rbac.yaml`, `daemonset.yaml`, `controller-deployment.yaml` |
| `e2e/` | 15 | `//go:build e2e` end-to-end test (`TestTimeshiftDate`) |
| metrics wiring | 19 | `cmd/controller/metrics.go`: Prometheus registry, 5 metrics; `/metrics` route via `promhttp`; per-route `track` middleware |

---

### `pkg/faketime`

**Module**: `github.com/bkaznowski/epochd/pkg/faketime` (standalone `go.mod` at `pkg/faketime/go.mod`; `replace github.com/bkaznowski/epochd => ../..` for local development)
**Import**: `import "github.com/bkaznowski/epochd/pkg/faketime"` — `go get github.com/bkaznowski/epochd/pkg/faketime` for external consumers
**File**: `pkg/faketime/faketime.go` — `//go:build linux`

Wraps `pkg/inject` for use in Go tests and CLI tooling without a Kubernetes cluster or
agent daemon.

#### `Handle` — single-process injection

```go
// Constructors — FollowChild path (no elevated perms, child must Ptrace: true):
func Start(cmd *exec.Cmd, target time.Time) (*Handle, error)       // advancing
func StartFrozen(cmd *exec.Cmd, target time.Time) (*Handle, error) // frozen

// Constructors — PTRACE_ATTACH path (CAP_SYS_PTRACE + ptrace_scope ≤ 1):
func Attach(pid int, target time.Time) (*Handle, error)
func AttachFrozen(pid int, target time.Time) (*Handle, error)

// Child-tracking constructors (phase 39) — keep ptrace attached after injection
// so the returned ChildTracker can watch for fork/exec events in the background.
// No elevated permissions required (FollowChild path).
func StartWithTracking(cmd *exec.Cmd, target time.Time) (*ChildTracker, error)
func StartFrozenWithTracking(cmd *exec.Cmd, target time.Time) (*ChildTracker, error)

// Update methods (all use process_vm_writev — no ptrace stop):
func (h *Handle) SetTime(target time.Time) error   // switch to advancing mode
func (h *Handle) Freeze(target time.Time) error    // switch to frozen mode
func (h *Handle) Advance(d time.Duration) error    // shift by delta, preserve mode
func (h *Handle) Reset() error                      // snap back to real clock
```

`Handle` tracks its own `offset`/`frozenAt`/`frozen` state (under a `sync.Mutex`) so
`effectiveTime()` can report the live fake time without re-reading process memory.

#### `ChildTracker` — automatic fork + exec tracking (phases 39–40)

`ChildTracker` keeps ptrace attached to the parent after injection and runs a
background goroutine that watches for `PTRACE_EVENT_FORK`/`PTRACE_EVENT_VFORK` and
`PTRACE_EVENT_EXEC` notifications.

```go
type ChildTracker struct {
    // Handle is the parent process's fake-time handle.
    Handle *Handle
    // unexported: tracer, parentPID, children map, pendingStop map, done chan, wg, loopErr
}

// Children returns Handles for all child processes currently tracked.
func (c *ChildTracker) Children() []*Handle

// Err returns the first error the background watch loop encountered, if any.
func (c *ChildTracker) Err() error

// Close stops the watch loop, resets all tracked children to the real clock,
// and detaches ptrace from the parent and all children.
func (c *ChildTracker) Close() error
```

**Fork handling**: `PTRACE_EVENT_FORK`/`PTRACE_EVENT_VFORK` → read child PID via
`GetEventMsgPID` → call `inject.ChildHandle` (same `StateAddr`, new PID — no re-injection
needed because fork copies the address space including the trampoline page) → add to
`children` and `pendingStop` → resume parent. When the child's implicit SIGSTOP arrives,
arm it with `SetOptionsPID(TRACEFORK|TRACEVFORK|TRACEEXEC)` → resume child.

**Exec handling** (phase 40): `PTRACE_EVENT_EXEC` → look up the Handle for `pid` →
call `inject.ReInjectAtTimeAfterExec` / `inject.ReInjectFrozenAfterExec` (full fresh
injection — vDSO locate, mmap trampoline, PokeText, write state) → swap `h.h` to the new
`inject.Handle` so future `SetTime`/`Freeze` calls write to the new trampoline.

**Self-exec** (PEX bootstrap, `os.Execv`): if `pid == c.parentPID`, the same logic runs
on `c.Handle` rather than a child entry.

#### `Session` — multi-process injection

```go
func NewSession(target time.Time) *Session

func (s *Session) Start(cmd *exec.Cmd) error      // add process, respects current mode
func (s *Session) Attach(pid int) error            // add already-running process

func (s *Session) SetTime(target time.Time) error  // update all handles concurrently
func (s *Session) Freeze(target time.Time) error   // freeze all handles concurrently
func (s *Session) Advance(d time.Duration) error   // shift all handles concurrently
func (s *Session) Reset() error
func (s *Session) Len() int
```

`Session.Start` / `Session.Attach` inject new processes at `effectiveTarget()` (the live
computed time), not at the original target, so processes added later see the correct time.
`applyAll` fans out `fn` to all handles concurrently, collects errors with `errors.Join`,
and only commits the new state via `updateState()` if all handles succeed.

---

### `pkg/sdk` — key additions in phases 34–37

```go
// Freeze mode:
func (c *Client) CreateFrozenTimeshift(ctx, ns, labelSelector string, target time.Time, ttl time.Duration) (*Timeshift, error)
func (c *Client) FreezeTimeshift(ctx, id string, target time.Time) (*Timeshift, error)

// Advance by duration:
func (c *Client) AdvanceTimeshift(ctx, id string, d time.Duration) (*Timeshift, error)

// Test helpers:
func WithFrozenTime(t *testing.T, labelSelector string, target time.Time, fn func())
```

`Timeshift.Time` is now the **live effective time** — for advancing timeshifts it grows
every time `GetTimeshift` is called; for frozen timeshifts it is constant.

### Controller internals — offset model (phase 36)

The `timeshift` struct stores:
- `offset time.Duration` — for advancing mode: `offset = target - now` at creation; `effectiveTime() = time.Now() + offset`
- `frozenAt time.Time` — for frozen mode: constant absolute timestamp
- `frozen bool` — which field is active

Stored to ConfigMap (`cmd/controller/store.go`) as:
- `Offset int64` (nanoseconds)
- `FrozenAt time.Time`

`advanceTimeshift(ctx, id, delta, freeze)` adds `delta` to the current effective time and
calls `updateTimeshift`, which then calls `SetTime` (or `Inject` with freeze) on all agent handles.

`PATCH /timeshifts/{id}` accepts either `time` (absolute RFC3339) or `duration` (Go
duration string) but not both. The watcher (`watcher.go`) recomputes `effectiveTarget()`
when re-injecting pods that restart, so newly added processes always see the current
offset-based time.

### Proto — `freeze` field (phase 37)

`proto/agent/v1/agent.proto` has `bool freeze = 3` on both `InjectRequest` and
`SetTimeRequest`. The freeze flag is no longer in gRPC metadata; it is a proper proto
field. The generated `pkg/agentpb/agent.pb.go` has `Freeze bool` with
`protobuf:"varint,3,opt,name=freeze,proto3"` and a `GetFreeze() bool` getter on both
message types.

The proto was regenerated using `bufbuild/buf` Docker image with `buf.gen.yaml` and
`buf.yaml` (kept in repo for future re-generation).

---

## Decisions and constraints to carry forward

### Time is always an absolute timestamp on the wire

All external APIs (HTTP, gRPC, CLI) use absolute RFC3339 timestamps, not raw offsets.
The `duration`-based advance path in `PATCH /timeshifts/{id}` is the one exception: the
client sends a relative duration, but the controller converts it to an absolute target
before forwarding to the agent; the agent still receives an absolute `google.protobuf.Timestamp`.

Internally, the controller stores a `time.Duration offset = target - now` (advancing mode)
or `time.Time frozenAt` (frozen mode) rather than the absolute target, so that
`GET /timeshifts/{id}` returns the live effective time (`time.Now() + offset`) instead of
a stale snapshot.

The agent is the last hop before the memory write. Converting `target - time.Now()` there
gives the most accurate offset; network latency at the controller is not included.
`inject.InjectAtTime` and `(*Handle).SetTime` both call `time.Now()` internally.

### The agent, not the controller, does the time-to-offset conversion

The agent is the last hop before the memory write. Converting `target - time.Now()` there
gives the most accurate offset. If the controller did it, network latency (tens to hundreds
of milliseconds) would corrupt the target's perceived time. `inject.InjectAtTime` and
`(*Handle).SetTime` already enforce this — they call `time.Now()` internally.

### `SetTime` is free after first injection

`(*Handle).SetTime` uses only `process_vm_writev` — no ptrace, no stop, one syscall.
Cheap enough to call from an HTTP handler or a background goroutine without concern. The
agent should hold `*inject.Handle` values in an in-memory map and call `SetTime` directly
for updates.

### Handle map lives in the agent, not the controller

The controller is stateless with respect to injection. It holds handle IDs (opaque strings
or UUIDs), but the actual `*inject.Handle` (which contains the `StateAddr`) lives in the
agent that owns the target process's node. The agent must persist the handle map for the
life of its process; there is no restart-safe persistence in v1.

---

## Phases 7–10 (implemented — kept for reference)

### Phase 7 — Node agent (`cmd/agent`, `pkg/api`)

**`pkg/api`** — shared wire types (no build tag, so controller and agent can both import):

```go
// Suggested types — adjust as needed.

type InjectRequest struct {
    ContainerID string `json:"containerID"`
    Time        string `json:"time"` // RFC3339
}

type InjectResponse struct {
    HandleID string `json:"handleID"`
}

type SetTimeRequest struct {
    HandleID string `json:"handleID"`
    Time     string `json:"time"` // RFC3339
}

type StatusResponse struct {
    HandleID    string `json:"handleID"`
    TargetTime  string `json:"targetTime"`  // RFC3339, what SetTime was last called with
    StateAddr   string `json:"stateAddr"`   // hex, for debugging
    Generation  uint32 `json:"generation"`
}
```

**`cmd/agent/main.go`** — the privileged DaemonSet pod:

1. **Container ID → PID resolution**: call the CRI socket (`/run/containerd/containerd.sock`)
   via `ContainerStatus` (from `k8s.io/cri-api`). That gives you the container's init PID,
   which is the target for injection. (Alternative first pass: parse `/proc/<pid>/cgroup`
   to find the container ID and do a reverse lookup via CRI, or use the ContainerID from
   the pod spec directly.) Put this in `pkg/k8sresolve`.

2. **HTTP or gRPC server**: for a faster first pass, use plain HTTP+JSON. Endpoints:
   - `POST /inject` — calls `inject.InjectAtTime(pid, target)`, stores handle in map,
     returns `HandleID`
   - `POST /settime` — looks up `HandleID` → `*inject.Handle`, calls `h.SetTime(target)`
   - `GET /status/{handleID}` — returns current generation, target time, state address

3. **In-memory handle map**: `map[string]*inject.Handle` protected by a `sync.RWMutex`.
   HandleIDs can be UUIDs (`crypto/rand`-based) or `"<containerID>:<pid>"` strings.

4. **Listening address**: bind to pod IP (or `0.0.0.0`) on a fixed port (e.g. 9100).
   The controller will reach it directly by pod IP.

**Important**: the agent binary requires `CAP_SYS_PTRACE` and `hostPID: true` in the pod
spec (phase 9). Running without these, `inject.InjectAtTime` will fail with EPERM on
`PTRACE_ATTACH`.

**New dependency needed**: `k8s.io/cri-api` (for CRI gRPC client). Add to go.mod when
implementing phase 7. Alternatively, for a simpler first pass: look up the PID by
scanning `/proc/*/cgroup` and matching the container ID string — no extra dependency.

**`pkg/k8sresolve`**: suggested signature:
```go
// LookupPID returns the init PID for the given container ID by calling the CRI socket.
func LookupPID(containerID, criSocket string) (int, error)
```

---

### Phase 8 — Controller (`cmd/controller`, `pkg/k8sresolve`)

The controller is an ordinary (unprivileged) Deployment. It:

1. **Resolves pods** using `client-go`: given a namespace and label selector, calls
   `k8s.io/client-go/kubernetes.CoreV1().Pods(ns).List(...)` to get pod objects.
   Extracts from each pod:
   - `.status.hostIP` — the node IP, used to reach the per-node agent
   - `.status.containerStatuses[].containerID` — passed to the agent for CRI lookup

2. **Maintains a node-IP → agent-URL map**: agents are DaemonSet pods, so there's one per
   node. The agent's pod IP is discoverable from the pod list; alternatively, use a fixed
   port on the node IP (`http://<hostIP>:9100`).

3. **REST API**:

```
POST   /skews
Body:  { "namespace": "...", "labelSelector": "app=foo", "time": "2030-01-01T00:00:00Z", "ttl": "1h" }
→ 201  { "id": "<uuid>", "appliedTo": ["pod/foo-abc", ...] }

PATCH  /skews/{id}
Body:  { "time": "2030-06-01T00:00:00Z" }
→ 200  { "id": "...", "time": "...", "appliedTo": [...], "expiresAt": "..." }

DELETE /skews/{id}
→ 204  (calls SetTime(now) on all handles, removes from registry)

GET    /skews/{id}
→ 200  { "id": "...", "time": "...", "appliedTo": [...], "expiresAt": "..." }
```

4. **TTL sweeper**: background goroutine; every 30s (or on TTL expiry) calls
   `SetTime(time.Now())` via the agent for expired skews, then removes them from the
   registry.

5. **Skew registry**: in-memory `map[string]*Skew`, where `Skew` holds the original
   request, the handle IDs per pod (as returned by the agent's `/inject`), and `expiresAt`.

**New dependencies needed**: `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`.

---

### Phase 9 — Kubernetes manifests (`deploy/`)

**`deploy/daemonset.yaml`** (agent):
```yaml
# Key fields — write a complete manifest around these:
spec:
  template:
    spec:
      hostPID: true                        # required — agent needs to target any pod's PID
      serviceAccountName: epochd-agent
      containers:
      - name: agent
        image: <registry>/epochd-agent:latest
        securityContext:
          capabilities:
            add: ["SYS_PTRACE"]            # required for PTRACE_ATTACH
        volumeMounts:
        - name: cri-socket
          mountPath: /run/containerd/containerd.sock
        ports:
        - containerPort: 9100
      volumes:
      - name: cri-socket
        hostPath:
          path: /run/containerd/containerd.sock
          type: Socket
```

**`deploy/rbac.yaml`**:
```yaml
# ServiceAccount for agent (needs no k8s API access beyond what's implicit)
# ServiceAccount for controller (needs get/list/watch on pods)
# ClusterRole + ClusterRoleBinding for controller's pod read access
```

**`deploy/controller-deployment.yaml`**:
- Normal unprivileged Deployment + ClusterIP Service (port 8080 or similar)
- Namespace: `epochd-system` (or `faketime-system` — pick one and be consistent)

---

### Phase 10 — e2e test SDK

A thin Go client library. Suggested package: `pkg/sdk` or a separate module at
`github.com/bkaznowski/epochd/sdk`.

```go
// WithTime sets the clock for all pods matching selector to target, runs fn, then
// resets to real time. Restores even if fn panics.
func WithTime(t *testing.T, selector string, target time.Time, fn func()) {
    id, err := client.CreateSkew(context.Background(), CreateSkewRequest{
        Namespace:     currentNamespace(),
        LabelSelector: selector,
        Time:          target,
        TTL:           30 * time.Minute,
    })
    if err != nil {
        t.Fatalf("epochd.WithTime: %v", err)
    }
    defer client.DeleteSkew(context.Background(), id)
    fn()
}
```

The `Client` wraps the controller's HTTP API. `DeleteSkew` calls `SetTime(now)` on the
controller side, which propagates to all agents.

---

## Testing patterns established in phases 1–6

### Docker test invocation

```
docker run --rm \
  --cap-add SYS_PTRACE \
  --security-opt seccomp=unconfined \
  -v "$(pwd):/workspace" -w /workspace \
  golang:1.26-alpine \
  go test ./... -count=1
```

Both flags are needed. `SYS_PTRACE` alone is not enough in Docker due to the default
seccomp profile blocking ptrace-related syscalls.

### In-binary helper processes

Tests that need a live target spawn the test binary itself:
```go
exe, _ := os.Executable()
cmd := exec.Command(exe, "-test.run=TestMyHelper", "-test.v")
cmd.Env = append(os.Environ(), "MY_HELPER_ENV=1")
cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
```
The helper function checks the env var and calls `t.Skip()` if unset, so it is a no-op in
normal test runs. This avoids needing pre-built binaries and makes the test fully
self-contained.

### Test structure for inject-level tests

- All inject tests use `FollowChild` (not `Attach`) to be Docker-compatible.
- `startPtraceChild(t, helperVal, extraArgs...)` in `inject_test.go` is the shared helper
  for tests that don't need stdout capture. Tests that need stdout (like `TestInjectObserved`
  and `TestInjectRoundTrip`) set up the pipe manually before `cmd.Start()`.
- `t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })` pattern prevents leaked children.

---

## Known limitations and future work

1. **No `Uninstall`**: `Handle.origBytes` stores the 5 original vDSO bytes but there is
   no `Uninstall()` method that uses them. The `SetTime(time.Now())` approach is sufficient
   for testing. A future `Uninstall()` would PokeText those bytes back and optionally
   `munmap` the trampoline page via the same `remoteMmap` mechanism (but with `SYS_MUNMAP`).

2. **Single process per injection**: Each `Handle` targets one PID. Containers with
   multiple processes (e.g. sidecars) each need their own `InjectAtTime` call. The
   controller should inject into the init PID (PID 1 in the container namespace), which is
   the parent of all processes in the container; all forked children inherit the same vDSO
   mapping and will see the same patched `clock_gettime`.

3. **No persistence across agent restart**: The handle map is in-memory. If the agent pod
   restarts, all injection state is lost. The controller should detect this (via a health
   endpoint or handle-not-found error) and re-inject.

4. **Race on state struct write**: `SetTime` uses plain word stores; there's no seqlock or
   atomic. A `clock_gettime` call concurrent with a `SetTime` can observe a torn state.
   For test workloads this is harmless (one slightly wrong timestamp). Adding a seqlock to
   the assembly would eliminate this at the cost of 3–4 more instruction bytes.

5. **x86-64 only**: The trampoline is hand-assembled. AArch64 (arm64) would need a
   different payload. The `JMP rel32` approach doesn't exist on AArch64 (`B` has 26-bit
   offset, ~128 MB reach). Alternative: patch with `LDR x16, [pc+8]; BR x16; .quad addr`
   (12 bytes, reach = full 64-bit space) — requires saving 12 bytes instead of 5.

6. **`CLOCK_REALTIME` only**: `CLOCK_MONOTONIC`, `CLOCK_BOOTTIME` are not intercepted.
   Adding them requires checking `clk_id` against a bitmask (the `enabledMask` field in
   the state struct already provides this — bit 0 = `CLOCK_REALTIME`, bit 1 could be
   `CLOCK_MONOTONIC = 1`, etc.) and the trampoline normalisation logic would need to run
   for each enabled clock.

---

## File tree as of phase 40

```
epochd/
├── go.mod                                     # module epochd, go 1.26.4
├── go.sum
├── buf.gen.yaml                               # bufbuild/buf remote plugin config (proto regen)
├── buf.yaml                                   # bufbuild/buf module config (proto regen)
├── plan.md                                    # original rollout plan (phases 0–19)
├── README.md                                  # user-facing docs
├── CONTEXT.md                                 # this file
├── TODO.md                                    # one-time setup + remaining phases
├── FUTURE.md                                  # longer-horizon improvements
├── Makefile                                   # cluster/load/deploy/e2e targets (kind)
├── Dockerfile.agent                           # scratch image for cmd/agent
├── Dockerfile.controller                      # scratch image for cmd/controller
├── .golangci.yml                              # errcheck, staticcheck, unused, govet
│
├── .github/
│   └── workflows/ci.yml                       # test + lint + build-images jobs
│
├── cmd/
│   ├── faketimectl/
│   │   ├── main.go                            # ✅ subcommand dispatch + usage
│   │   ├── cmds_controller.go                 # ✅ create/list/get/update/advance/delete/status/resolve
│   │   └── cmds_inject.go                     # ✅ inject/reset (local ptrace, linux-only)
│   ├── agent/main.go                          # ✅ gRPC daemon: Inject/SetTime/Reset + handle map; freeze via req.Freeze field
│   └── controller/
│       ├── main.go                            # ✅ flags, k8s client, agent pool, HTTP server
│       ├── controller.go                      # ✅ timeshift registry (offset model), CRUD, advance, sweeper
│       ├── store.go                           # ✅ ConfigMap persistence; stores Offset (int64 ns) + FrozenAt
│       ├── handlers.go                        # ✅ HTTP routes; PATCH accepts time or duration (mutually exclusive)
│       ├── metrics.go                         # ✅ Prometheus registry + 5 metrics
│       ├── watcher.go                         # ✅ SharedInformer pod watcher; recomputes effectiveTarget() on re-inject
│       └── controller_test.go                 # ✅ unit tests including TestHTTPAdvanceTimeshift
│
├── pkg/
│   ├── vdso/
│   │   ├── vdso.go                            # ✅ Locate(pid) → VDSOInfo
│   │   └── locate_test.go                     # ✅
│   ├── procmem/
│   │   ├── procmem.go                         # ✅ Tracer, ReadMem, WriteMem, PokeText, FollowChild; SetTracee, SetOptions, SetOptionsPID, WaitAnyNonBlocking, GetEventMsgPID, ContPID, InterruptDetach, DetachAll
│   │   └── procmem_test.go                    # ✅ TestTracerBasic
│   ├── trampoline/
│   │   ├── trampoline.asm                     # ✅ NASM source (118 bytes); MaskEnabled=1, MaskFrozen=3
│   │   ├── trampoline.bin                     # ✅ assembled binary (committed)
│   │   ├── trampoline.go                      # ✅ Payload, StateOffset=86, EncodeState, DecodeState, MaskEnabled, MaskFrozen
│   │   └── trampoline_test.go                 # ✅ regression + round-trip tests
│   ├── inject/
│   │   ├── inject.go                          # ✅ InjectAtTime, InjectFrozen, InjectAtTimeFollowChild, InjectFrozenFollowChild; InjectAtTimeFollowChildKeepTracer, InjectFrozenFollowChildKeepTracer, ChildHandle, ReInjectAtTimeAfterExec, ReInjectFrozenAfterExec
│   │   ├── inject_test.go                     # ✅ TestRemoteMmap, TestInjectMechanics (uses writeState + MaskEnabled), TestInjectObserved*
│   │   └── roundtrip_test.go                  # ✅ TestInjectRoundTrip* (inject+verify+reset+verify; uses MaskEnabled)
│   ├── faketime/                              # standalone module: github.com/bkaznowski/epochd/pkg/faketime
│   │   ├── faketime.go                       # ✅ Start, StartFrozen, Attach, AttachFrozen, Handle, Session, ChildTracker, StartWithTracking, StartFrozenWithTracking
│   │   ├── faketime_stub.go                  # ✅ non-Linux stubs (t.Skip() / errNotSupported); includes ChildTracker stubs
│   │   ├── example_test.go                   # ✅ niladic Example funcs (ExampleStart, ExampleWithProcess, ExampleWithSession, ExampleStartWithTracking)
│   │   ├── bench_test.go                     # ✅ BenchmarkClockGettime (faked vs baseline ns/clock_gettime via subprocess)
│   │   └── go.mod                            # module github.com/bkaznowski/epochd/pkg/faketime; replace github.com/bkaznowski/epochd => ../..
│   ├── agentpb/                               # ✅ generated from proto/agent/v1/agent.proto (freeze=field 3)
│   │   └── agent_grpc.pb.go, agent.pb.go
│   ├── agentclient/
│   │   └── agentclient.go                     # ✅ Pool: per-node gRPC connections; freeze via req.Freeze field (not metadata)
│   ├── k8sresolve/
│   │   └── k8sresolve.go                      # ✅ LookupPID: container ID → PID via /proc/*/cgroup
│   ├── api/
│   │   └── api.go                             # ✅ UpdateTimeshiftRequest has Time, Duration, Freeze fields (no build tag)
│   └── sdk/
│       ├── sdk.go                             # ✅ CreateTimeshift, CreateFrozenTimeshift, FreezeTimeshift, AdvanceTimeshift, …
│       ├── testing.go                         # ✅ WithTime, WithFrozenTime
│       └── sdk_test.go                        # ✅ fake-server tests
│
├── proto/
│   └── agent/v1/
│       └── agent.proto                        # ✅ InjectRequest + SetTimeRequest both have bool freeze = 3
│
├── test/
│   └── targets/
│       └── clockprinter/main.go               # ✅ prints time.Now() every second
│
├── e2e/
│   └── e2e_test.go                            # ✅ //go:build e2e; TestTimeshiftDate (kind cluster)
│
└── deploy/
    ├── rbac.yaml                              # ✅ ServiceAccount + ClusterRole for controller
    ├── daemonset.yaml                         # ✅ agent: hostPID, SYS_PTRACE, CRI socket
    └── controller-deployment.yaml            # ✅ controller Deployment + ClusterIP Service + /healthz probe
```

\* `TestInjectObserved` is gated on `EPOCHD_INJECT_E2E=1` (skipped in CI; some environments consume the ptrace exec-stop before `FollowChild` can catch it). `TestInjectRoundTrip` runs unconditionally.
