# epochd

[![CI](https://github.com/bkaznowski/epochd/actions/workflows/ci.yml/badge.svg)](https://github.com/bkaznowski/epochd/actions/workflows/ci.yml)
[![Coverage](https://bkaznowski.github.io/epochd/coverage/badge.svg)](https://bkaznowski.github.io/epochd/coverage/)
[![Go 1.26](https://img.shields.io/badge/go-1.26-blue.svg)](https://go.dev/dl/)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A Go tool that injects a fake wall-clock time into a running Linux process without
restarting it, stopping it for more than a moment, or modifying its binary. Designed for
Kubernetes end-to-end tests that need to control time on specific pods (e.g. testing
certificate expiry, token refresh, cron scheduling).

Two clock modes are supported:

- **Advancing** — the target's clock is shifted by a fixed offset from real time. The clock
  keeps advancing at the normal rate; `time.Now()` returns `real_now + offset`.
- **Frozen** — the target's clock is pinned to a specific instant. Every call to
  `clock_gettime` returns exactly that time, regardless of how much real time passes.

After a one-time injection, updating the time requires a single `process_vm_writev`
syscall — no `ptrace` stop, no signal, no coordination with the target.

**Platform**: Linux x86-64 only. Everything in the codebase is guarded with
`//go:build linux`.

---

## How it works

### The problem with patching time at the syscall level

The naive approach — intercept every `clock_gettime` syscall with ptrace — requires
stopping the process for each call. Go's runtime calls `clock_gettime` tens of thousands of
times per second. That's unusable.

### The vDSO shortcut

Modern Linux maps a small shared library called the **vDSO** ("virtual dynamic shared
object") into every process's address space. Glibc's wall-clock functions don't issue a
syscall at all — they call symbols in the vDSO, which reads the kernel's timekeeping data
from a shared memory page directly. This is why `clock_gettime` is typically ~20 ns instead
of ~200 ns.

Because the vDSO is a normal mapped region, its code is writable under `PTRACE_POKETEXT`
(which can write read-only-but-executable pages, exactly as debuggers write breakpoints).

**Three independent functions need patching, not one.** `clock_gettime`, `gettimeofday`, and
`time` are three separate compiled functions at three different addresses in the vDSO — they
are not aliases of each other, even though they all report the same wall clock. Patching only
`clock_gettime` (an earlier version of this tool did) silently misses anything that calls the
other two — notably PostgreSQL's `GetCurrentTimestamp()` and glibc/bash's own wall-clock reads
(`$EPOCHREALTIME`), both of which call `gettimeofday`. All three are patched today, each with
its own small stub, sharing one state struct so a single `SetTime`/`Freeze` call updates all of
them consistently.

### The injection sequence

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Target process address space                                           │
│                                                                         │
│  [vdso]   0x7fff....a40  ← clock_gettime entry point   ─┐               │
│  [vdso]   0x7fff....7a0  ← gettimeofday entry point    ─┼─┐             │
│  [vdso]   0x7fff....a10  ← time entry point            ─┼─┼─┐           │
│    │  Before: original vDSO code                        │ │ │           │
│    │  After:  E9 xx xx xx xx  ← JMP rel32, one per entry│ │ │           │
│    └───────────────────────────────────────────────────▼─▼─▼            │
│                                                                         │
│  [anon rwx page, allocated by the target itself via mmap]               │
│    ├─ clock_gettime stub  (offset 0)                                    │
│    │    intercepts clk_id ∈ {CLOCK_REALTIME, CLOCK_REALTIME_COARSE};    │
│    │    real syscall → add offsetSec/offsetNsec → normalise tv_nsec     │
│    ├─ gettimeofday stub   (offset 141)                                  │
│    │    same shape, tv_usec instead of tv_nsec (offsetNsec / 1000)      │
│    ├─ time stub           (offset 306)                                  │
│    │    real clock_gettime(CLOCK_REALTIME) internally (not the time()   │
│    │    syscall, which discards the real fractional second before this  │
│    │    stub would ever see it) → same normalised add → return tv_sec   │
│    └─ shared state struct (32 bytes, at StateOffset = 436)              │
│         +0   int64  offsetSec                                           │
│         +8   int64  offsetNsec                                          │
│         +16  uint64 enabledMask  (1=advancing, 3=frozen)                │
│         +24  uint32 generation   (bumped on each SetTime/Freeze)        │
│         +28  uint32 _pad                                                │
└─────────────────────────────────────────────────────────────────────────┘
```

All three stubs live in one allocated page and share one state struct, so a single
`SetTime`/`Freeze`/`Advance` call updates the fake time for all three functions at once —
`clock_gettime`, `gettimeofday`, and `time` in a target process always agree with each other
to the second.

**Step by step** (see `pkg/inject/inject.go`):

1. **vDSO discovery** — parse `/proc/<pid>/maps` for `[vdso]`, read those bytes via
   `/proc/<pid>/mem`, parse with `debug/elf`, resolve the `clock_gettime`, `gettimeofday`,
   and `time` symbols → three absolute addresses (`pkg/vdso.VDSOInfo`).

2. **Remote mmap** — while the target is ptrace-stopped, temporarily overwrite three
   bytes at the `clock_gettime` entry (used as a scratch location; it isn't touched again
   until step 4) with `0F 05 CC` (`syscall; int3`), set registers for
   `mmap(hint, 4096, PROT_RWX, MAP_PRIVATE|MAP_ANON|MAP_FIXED_NOREPLACE, -1, 0)`,
   resume, wait for the `int3` SIGTRAP, read `RAX` for the new page address, restore
   original bytes and registers. The hint is chosen by scanning `/proc/<pid>/maps` for
   the nearest free page within ±2 GB of the vDSO entry (required for `JMP rel32` reach) —
   all three hooked functions live in the same few-KB vDSO mapping, so one page is in reach
   of all three.

3. **Write trampoline** — copy the embedded binary payload (all three stubs + shared
   state) into the new page with `offsetSec`/`offsetNsec` already set. Use
   `process_vm_writev` (no ptrace stop needed for writeable pages).

4. **Patch vDSO** — for each of the three functions, write `E9 <disp32>` over its first 5
   bytes using `PTRACE_POKETEXT` (the only way to write a read-only mapped page), targeting
   that function's own stub offset in the trampoline page.

5. **Detach** — the target resumes. Every subsequent `clock_gettime`, `gettimeofday`, or
   `time` call now goes through the corresponding trampoline stub.

6. **Update time** — `SetTime` / `Freeze` writes a new state struct (32 bytes) into the
   trampoline page using `process_vm_writev`. No ptrace needed. The `enabledMask` field
   controls behaviour: `MaskEnabled = 1` (bit 0 set) means advancing mode — each stub adds
   `(offsetSec, offsetNsec)` to the real time. `MaskFrozen = 3` (bits 0+1 set) means
   freeze mode — the offsets encode an absolute timestamp; each stub ignores the real
   time and returns that value directly. The stubs read the state with plain loads;
   a concurrent update and a clock read can race — the worst outcome is one call
   returning a time between the old and new values, which is acceptable for testing.

---

## Package layout

```
epochd/
├── cmd/
│   ├── agent/            # Node-level gRPC daemon (runs as DaemonSet)
│   ├── controller/       # Control-plane HTTP+JSON API
│   └── faketimectl/      # Manual CLI for local testing
│
├── pkg/
│   ├── vdso/             # vDSO discovery and ELF symbol resolution
│   ├── procmem/          # ptrace wrapper + process_vm_readv/writev
│   ├── trampoline/       # Assembled payload bytes + state struct helpers
│   ├── inject/           # Injection orchestration; public API
│   ├── faketime/         # Non-Kubernetes injection (standalone module: github.com/bkaznowski/epochd/pkg/faketime)
│   ├── agentpb/          # Generated gRPC types (agent.proto)
│   ├── agentclient/      # gRPC connection pool (controller → agents)
│   ├── k8sresolve/       # Container ID → PID resolution via /proc
│   ├── api/              # Shared HTTP request/response types
│   └── sdk/              # Go client library for e2e tests
│
├── proto/
│   └── agent/v1/         # Protobuf source for the agent gRPC API
│
├── test/
│   └── targets/
│       └── clockprinter/ # Sample target: prints time.Now() every second
│
├── deploy/               # Kubernetes manifests
│   ├── rbac.yaml
│   ├── daemonset.yaml
│   └── controller-deployment.yaml
│
├── Dockerfile.agent
└── Dockerfile.controller
```

---

## Prerequisites

| Tool | Purpose |
|------|---------|
| Go 1.26+ | Build and test |
| Linux x86-64 | Runtime (vDSO hook is arch-specific) |
| Docker | Run tests in a Linux container when developing on macOS/Windows |

The compiled `trampoline.bin` (vDSO mode) and `libepochd_preload.so` (preload mode) are
committed to the repo, so `go build` needs no C toolchain. Docker is only needed to rebuild
them after editing their C sources (`pkg/trampoline/src/build.sh`,
`pkg/preload/lib/build.sh`).

---

## Running the tests

### In Docker (recommended — works on macOS and Windows)

```bash
docker run --rm \
  --cap-add SYS_PTRACE \
  --security-opt seccomp=unconfined \
  -v "$(pwd):/workspace" -w /workspace \
  golang:1.26-alpine \
  go test ./... -count=1
```

`SYS_PTRACE` and `seccomp=unconfined` are required because the tests use `ptrace`. The
test binary spawns child processes with `SysProcAttr{Ptrace: true}` (the child calls
`PTRACE_TRACEME`) to work around Docker's Yama `ptrace_scope=1` restriction, which blocks
`PTRACE_ATTACH` even with the capability granted.

### On a real Linux host

```bash
go test ./... -count=1
```

Requires either `root` or `CAP_SYS_PTRACE` and `ptrace_scope` ≤ 1:

```bash
echo 0 | sudo tee /proc/sys/kernel/yama/ptrace_scope
```

### Run a single package verbosely

```bash
# vDSO discovery
go test ./pkg/vdso/ -v

# ptrace primitives
go test ./pkg/procmem/ -v

# trampoline encoding/decoding
go test ./pkg/trampoline/ -v

# injection mechanics + round-trip integration
go test ./pkg/inject/ -v
```

### Key tests by name

| Test | Package | What it checks |
|------|---------|----------------|
| `TestLocateSelf` | `pkg/vdso` | Resolves `clock_gettime`, `gettimeofday`, and `time` in the test process's own vDSO |
| `TestTracerBasic` | `pkg/procmem` | Attach, ReadMem, WriteMem, PokeText, Detach |
| `TestStateOffsetRegression` | `pkg/trampoline` | `StateOffset` constant matches actual binary layout |
| `TestEntryOffsetRegression` | `pkg/trampoline` | Each hook stub's offset matches the assembled bytes at that address |
| `TestEncodeDecodeRoundTrip` | `pkg/trampoline` | `EncodeState` / `DecodeState` are inverses |
| `TestRemoteMmap` | `pkg/inject` | Remote mmap allocates a new rwx page in target |
| `TestInjectMechanics` | `pkg/inject` | State struct fields, JMP displacement, setOffset, child survives Detach |
| `TestInjectObserved` | `pkg/inject` | End-to-end: injected process prints timestamps ~24 h ahead |
| `TestInjectRoundTrip` | `pkg/inject` | Full cycle: inject +24 h → verify → SetTime(now) → verify reset |

---

## Building

```bash
# Build everything (Linux target)
CGO_ENABLED=0 GOOS=linux go build ./...

# Build individual binaries
CGO_ENABLED=0 GOOS=linux go build -o agent      ./cmd/agent
CGO_ENABLED=0 GOOS=linux go build -o controller ./cmd/controller
CGO_ENABLED=0 GOOS=linux go build -o faketimectl ./cmd/faketimectl
```

Cross-compiling from macOS or Windows: add `GOARCH=amd64` if your host is not x86-64.
The build is pure Go (`CGO_ENABLED=0`); no CGo. The trampoline is pre-assembled bytes in a `[]byte`.

### Docker images

```bash
# Agent (must be linux/amd64)
docker build -f Dockerfile.agent --platform linux/amd64 -t epochd-agent:dev .

# Controller
docker build -f Dockerfile.controller --platform linux/amd64 -t epochd-controller:dev .
```

Both images use `scratch` as the runtime base — no shell, no libc, ~10–50 MB total.

---

## `faketimectl` — manual testing CLI

`faketimectl` has two groups of subcommands: **controller subcommands** that talk to a
running epochd controller over HTTP, and **local injection subcommands** that directly
ptrace a process on the current machine (Linux only, requires `CAP_SYS_PTRACE`).

### Controller subcommands

Set `EPOCHD_URL` or pass `--url` on every command.

```
faketimectl create  --namespace=NS --selector=SEL --time=RFC3339 [--ttl=DUR] [--freeze]
faketimectl list
faketimectl get     <id>
faketimectl update  <id> --time=RFC3339 [--freeze]
faketimectl advance <id> --by=DURATION
faketimectl delete  <id>
faketimectl status  <id>
faketimectl resolve --namespace=NS --selector=SEL
```

| Subcommand | Description |
|------------|-------------|
| `create`   | Inject a fake time into all matching pods. `--freeze` pins the clock at `--time` so it never advances. `--ttl` auto-deletes after the given duration. |
| `list`     | List all active timeshifts. The `TIME` column reflects the live effective time the processes currently see. |
| `get`      | Print details for one timeshift, including the current effective time. |
| `update`   | Move the clock to a new absolute time. `--freeze` switches to/from freeze mode. |
| `advance`  | Shift the clock forward (or backward) by a Go duration. Preserves the current mode. |
| `delete`   | Reset all targeted processes to the real clock and remove the timeshift. |
| `status`   | Query each node agent for the live trampoline state (generation counter, PID, last write). |
| `resolve`  | Preview which pods and containers would be targeted without injecting anything. |

**Example — advance a frozen clock by one day**

```bash
export EPOCHD_URL=http://localhost:8080

# Create a timeshift frozen at 2030-01-01.
faketimectl create --namespace=default --selector=app=web \
  --time=2030-01-01T00:00:00Z --freeze
# created timeshift a1b2c3d4...
#   namespace:  default
#   time:       2030-01-01T00:00:00Z
#   frozen:     yes
#   applied to: web-abc/main

# An hour later in real time, the processes still see 2030-01-01T00:00:00Z.
faketimectl get a1b2c3d4
#   time:       2030-01-01T00:00:00Z

# Advance by one day.
faketimectl advance a1b2c3d4 --by=24h
#   time:       2030-01-02T00:00:00Z   ← frozen at the new point

# Switch to advancing mode and move time to 2030-06-01.
faketimectl update a1b2c3d4 --time=2030-06-01T00:00:00Z
#   time:       2030-06-01T00:00:02Z   ← now advancing; seconds tick forward

# Clean up.
faketimectl delete a1b2c3d4
```

### Local injection subcommands

These directly ptrace a process — no controller required.

```
faketimectl inject --pid=PID --time=RFC3339 [--freeze]
faketimectl reset  --pid=PID
```

| Flag | Description |
|------|-------------|
| `--pid` | Target process PID (required) |
| `--time` | Fake wall-clock time in RFC3339, e.g. `2030-01-01T00:00:00Z` |
| `--freeze` | Pin the clock at `--time` so it never advances |
| `--reset` | Snap the target back to the real clock |

**Example — inject a fake advancing time into a running process**

```bash
# Terminal 1: run the sample target
./clockprinter
# 2026-06-21T10:00:00Z
# 2026-06-21T10:00:01Z

# Terminal 2: inject +4 years, advancing
sudo faketimectl inject --pid=$(pgrep clockprinter) --time=2030-01-01T00:00:00Z

# Terminal 1 now prints:
# 2030-01-01T00:00:00Z
# 2030-01-01T00:00:01Z

# Terminal 2: reset to real time
sudo faketimectl reset --pid=$(pgrep clockprinter)
```

**Permissions**: local injection calls `PTRACE_ATTACH`, which requires `CAP_SYS_PTRACE`
(or `root`) and `ptrace_scope` ≤ 1:

```bash
echo 0 | sudo tee /proc/sys/kernel/yama/ptrace_scope
```

---

## The trampoline

### Source: `pkg/trampoline/src/trampoline.c`

The payload is freestanding C: no libc, no startup code, no stack protector. It contains
three independent entry points, one per hooked vDSO function, followed by one shared state
struct. `inject.go` JMP-patches each vDSO function's real entry point to the corresponding
`*_entry` symbol. The only assembly is the `syscall` instruction inside two tiny inline
helpers; register allocation, spilling and the System V calling convention are left to the
compiler.

The `clock_gettime` hook (see the file for `gettimeofday_entry` and `time_entry`, which
follow the same shape):

```c
int clock_gettime_entry(int clk_id, struct timespec *tp)
{
    if (clk_id != CLOCK_REALTIME && clk_id != CLOCK_REALTIME_COARSE)
        return (int)raw_syscall2(SYS_clock_gettime, clk_id, (long)tp);

    uint64_t mask = g_state.enabled_mask;
    if (!(mask & MASK_ENABLED))
        return (int)raw_syscall2(SYS_clock_gettime, clk_id, (long)tp);

    if (mask & MASK_FROZEN) {                 /* no real read at all */
        tp->tv_sec  = g_state.offset_sec;
        tp->tv_nsec = g_state.offset_nsec;
        return 0;
    }

    long rc = raw_syscall2(SYS_clock_gettime, clk_id, (long)tp);
    if (rc)
        return (int)rc;                       /* the real error, offset not applied */
    int64_t sec  = tp->tv_sec + g_state.offset_sec;
    int64_t nsec = tp->tv_nsec + g_state.offset_nsec;
    normalize(&sec, &nsec, NS_PER_SEC);       /* one step: offset_nsec is in (-1e9, 1e9) */
    tp->tv_sec  = sec;
    tp->tv_nsec = nsec;
    return 0;
}
```

State struct, shared by all three hooks and written by the Go layer with
`process_vm_writev`:

```c
struct state {
    int64_t  offset_sec;    /* +0   advancing: added to tv_sec;  frozen: absolute tv_sec  */
    int64_t  offset_nsec;   /* +8   advancing: added to tv_nsec; frozen: absolute tv_nsec */
    uint64_t enabled_mask;  /* +16  bit 0 = enabled, bit 1 = freeze mode                  */
    uint32_t generation;    /* +24  bumped on every SetTime/Freeze                        */
    uint32_t _pad;          /* +28                                                        */
};
```

Key design choices:
- **No call back into original vDSO code**: each hook issues a raw `syscall` to get the
  real time. This avoids saving, relocating and re-executing the original instruction
  bytes (the fiddly part of classic inline hooks). The cost is that an advancing clock pays
  a real syscall per read.
- **Position-independent**: every access to the state struct is RIP-relative, so the
  payload can be placed anywhere within the JMP's reach. The build fails if the object
  contains an absolute address, a GOT/PLT entry or a `call`.
- **`clock_gettime` intercepts `CLOCK_REALTIME` and `CLOCK_REALTIME_COARSE`** (same wall
  clock, the latter is just a faster/coarser read of it). `CLOCK_MONOTONIC`,
  `CLOCK_BOOTTIME`, etc. deliberately pass through unchanged, so internal
  timeout/latency logic in the target process (e.g. PostgreSQL's `statement_timeout`,
  lock waits) keeps seeing real elapsed time.
- **`gettimeofday` and `time` are also intercepted**, not just `clock_gettime`: they are
  independent functions at independent vDSO addresses, not aliases, and PostgreSQL's
  `GetCurrentTimestamp()` calls `gettimeofday`, not `clock_gettime`.
- **`time_entry` calls the real `clock_gettime(CLOCK_REALTIME)` internally, not the
  `time()` syscall**: `SYS_time` only ever returns whole seconds, discarding the real
  fractional second before the trampoline would ever see it, which could make `time()`
  disagree with `clock_gettime`/`gettimeofday` about the current second by up to 1s
  whenever `offset_nsec` was nonzero. Applying the same carry-normalised add keeps all
  three in agreement to the second.
- **Errors are the real errors.** A hook returns what the real vDSO function would: 0, or
  a negative errno such as `-EINVAL` for an unsupported clock or `-EFAULT` for a bad
  pointer. Arguments the hooks do not use (such as `gettimeofday`'s timezone pointer) are
  passed through to the kernel untouched.

### Building

```bash
pkg/trampoline/src/build.sh        # needs only Docker
```

The build compiles `trampoline.c` with `link.ld`, verifies the result is genuinely
position-independent, and writes four artifacts into `pkg/trampoline/`, all of which are
committed so that consumers need no C toolchain:

| File | Purpose |
|------|---------|
| `trampoline.bin` | The flat blob Go embeds |
| `trampoline.elf` | The linked ELF; tests read its symbol table |
| `offsets_generated.go` | `StateOffset` and the three `*EntryOffset` constants, read from the ELF's symbols rather than counted by hand |
| `trampoline.src.sha256` | Hash of `trampoline.c` and `link.ld` |

Nothing relies on a developer remembering to rebuild. `go test ./pkg/trampoline` checks
that each `*EntryOffset` and `StateOffset` equals its symbol's address in the ELF, that
`trampoline.bin` is the flat image of that ELF with `g_state` as its last 32 bytes, and
that the sources still hash to the recorded value, so editing the C without rebuilding
fails the test. A further set of tests maps the blob executable and calls the hooks
directly, covering carry and borrow, error propagation and argument passthrough without
ptrace or a target process.

### Embedding

```go
//go:embed trampoline.bin
var Payload []byte
```

The binary is embedded at build time. No runtime file I/O; no CGo.

---

## `pkg/inject` internals

### `findNearbyGap` — why a plain mmap hint isn't enough

`JMP rel32` has a range of ±2 GB. Anonymous `mmap(hint, ...)` asks the kernel to try
placing the region near `hint`, but the kernel is free to ignore it — and will, when the
address space near the vDSO is saturated (common in Docker where ASLR places the vDSO
near the top of userspace, leaving little room above it).

`findNearbyGap` reads `/proc/<pid>/maps` while the tracee is ptrace-stopped (so the map is
stable) and finds the first unmapped page-aligned gap within ±2 GB of the vDSO entry. That
address is then passed to `remoteMmap` with `MAP_FIXED_NOREPLACE`, which either lands
exactly there or fails with `EEXIST` (no silent fall-back to a far address).

### `remoteMmap` — making the target allocate its own page

The target calls `mmap` on its own behalf so the resulting page is in its own address
space. The sequence:

1. Save the tracee's registers and 8 bytes at `clock_gettime`.
2. Overwrite those bytes with `syscall; int3` (`0F 05 CC`).
3. Set `RIP = clock_gettime`, `RAX = SYS_mmap`, and argument registers.
4. `PTRACE_CONT` → wait for `SIGTRAP` from the `int3`.
5. Read the result from `RAX`.
6. Restore the original bytes and registers.

The `clock_gettime` vDSO entry is used as the scratch location for this one-time mmap call
because it is already known to be executable and reachable — `gettimeofday` and `time`
aren't touched here. The final `JMP rel32` patches happen later, once per hooked function
(`clock_gettime`, `gettimeofday`, `time`), after the trampoline has been written.

### `writeState` / `SetTime` / `Freeze` — live updates with no ptrace

After injection, updating the fake time requires only `process_vm_writev` into the state
struct. The target does not need to be stopped. A `generation` counter is incremented on
every write for observability (visible in `TestInjectMechanics`).

The public `Handle` API:

```go
// Advancing mode — trampoline adds (target - now) to real clock_gettime result.
func (h *Handle) SetTime(target time.Time) error

// Frozen mode — trampoline ignores real time and always returns exactly target.
func (h *Handle) Freeze(target time.Time) error
```

Internally, both call `writeState(sec, nsec, mask)` which writes a 32-byte state struct
via `process_vm_writev`. The mask controls mode: `MaskEnabled = 1` for advancing,
`MaskFrozen = 3` for frozen.

The four top-level constructors handle both modes and both ptrace paths:

```go
func InjectAtTime(pid int, target time.Time) (*Handle, error)       // Attach, advancing
func InjectFrozen(pid int, target time.Time) (*Handle, error)       // Attach, frozen
func InjectAtTimeFollowChild(pid int, target time.Time) (*Handle, error) // FollowChild, advancing
func InjectFrozenFollowChild(pid int, target time.Time) (*Handle, error) // FollowChild, frozen
```

### `Tracer` — OS-thread affinity for ptrace

Linux requires that every `ptrace` call for a given tracee come from the same OS thread
that issued `PTRACE_ATTACH`. Go's scheduler moves goroutines between OS threads freely,
which would break this. `Tracer` solves it by owning a single goroutine that calls
`runtime.LockOSThread()` at startup and never releases it. All ptrace operations are sent
as closures over a channel and executed on that pinned thread.

### `FollowChild` vs `Attach`

| | `FollowChild` | `Attach` |
|---|---|---|
| Mechanism | Child calls `PTRACE_TRACEME`; parent waits for SIGTRAP | Parent calls `PTRACE_ATTACH`; sends `SIGSTOP` |
| Requires | Owning the child process | `CAP_SYS_PTRACE` + `ptrace_scope ≤ 1` |
| Used in | Tests (Docker-compatible) | `faketimectl`, production agent |

---

## Freeze mode

When a timeshift is created or updated with `--freeze` / `freeze: true`, the trampoline
enters **frozen mode**: the `enabledMask` field is set to `MaskFrozen = 3` (bits 0 and 1),
and the offset fields store the absolute target timestamp rather than a delta. Every
`clock_gettime` (`CLOCK_REALTIME` or `CLOCK_REALTIME_COARSE`), `gettimeofday`, and `time`
call in the target returns exactly that timestamp, regardless of how much real time passes.

Freeze mode is useful for:
- Reproducing time-sensitive bugs that only trigger at a specific instant.
- Tests that need a deterministic, non-advancing clock (e.g. certificate expiry checks
  where the exact timestamp must match).
- Pausing time while performing setup, then advancing by discrete steps.

### SDK — freeze mode

```go
client := sdk.NewClient("http://localhost:8080")

// Create a frozen timeshift.
ts, err := client.CreateFrozenTimeshift(ctx, "default", "app=web",
    time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), 0)

// Switch an existing timeshift to frozen mode.
ts, err = client.FreezeTimeshift(ctx, ts.ID, time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC))

// Test helper — runs fn with the clock frozen, restores on return.
sdk.WithFrozenTime(t, "app=web", frozenAt, func() {
    // time.Now() in target pods always returns frozenAt
})
```

### `pkg/faketime` — freeze mode (local processes)

```go
// Start a child process with its clock frozen at target.
handle, err := faketime.StartFrozen(cmd, target)

// Attach to an already-running process.
handle, err = faketime.AttachFrozen(pid, target)

// Freeze/unfreeze via the handle.
handle.Freeze(newTarget)
handle.SetTime(advancingTarget) // switches back to advancing mode

// Session-level freeze.
session := faketime.NewSession(target)
session.Freeze(target)
session.Start(cmd) // new processes joined after Freeze() are also frozen

// Session with fork/exec tracking — children and exec'd processes are
// automatically injected. Close() shuts the watcher down.
session := faketime.NewSession(target, faketime.WithTracking())
defer session.Reset()
defer session.Close()
session.Start(cmd) // parent + all descendants see fake time
```

---

## Advance-by-duration

Instead of providing an absolute target time, you can advance (or rewind) the current fake
time by a relative amount. This works in both advancing and frozen modes and preserves the
current mode.

### HTTP API

```
PATCH /timeshifts/{id}
Content-Type: application/json

{ "duration": "24h" }
```

Accepted by `PATCH /timeshifts/{id}` alongside the existing `time` field (the two are
mutually exclusive). Go duration strings are supported (`"24h"`, `"-1h30m"`, `"72h"`).

The `time` field in `GET /timeshifts/{id}` responses always reflects the **live** effective
time — the actual timestamp the targeted processes currently see. For advancing timeshifts
this grows every second; for frozen timeshifts it is constant.

### CLI

```bash
faketimectl advance <id> --by=24h    # advance by 24 hours
faketimectl advance <id> --by=-1h    # rewind by 1 hour
```

### SDK

```go
// Shift forward by one day.
ts, err := client.AdvanceTimeshift(ctx, ts.ID, 24*time.Hour)
```

### `pkg/faketime` — advance (local processes)

```go
// Advance a single-process handle by one day.
err = handle.Advance(24 * time.Hour)

// Advance all processes in a session.
err = session.Advance(24 * time.Hour)
```

---

## Known limitations

- **x86-64 only.** The trampoline's JMP patch and `syscall` sequence are specific to `x86_64`. `aarch64` would need a
  different payload and a different JMP patch strategy (AArch64 `B` has only a 26-bit
  offset; you'd need an indirect branch via a scratch register instead).

- **Wall clock only — `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME` are not intercepted.**
  `clock_gettime`, `gettimeofday`, and `time` (all wall-clock reads) are all patched, and
  `clock_gettime` additionally covers `CLOCK_REALTIME_COARSE`. Monotonic/boottime clock IDs
  deliberately pass through unchanged, so internal timeout/latency logic in the target
  process (e.g. PostgreSQL's `statement_timeout`, lock waits) keeps seeing real elapsed
  time — shifting them would be surprising (see `FUTURE.md` for a design sketch if you want
  this as an opt-in).

- **No teardown in v1.** There is no `Uninstall` that restores the original vDSO bytes.
  Calling `SetTime(time.Now())` effectively resets the clock to real time, which is
  sufficient for test cleanup. The trampoline page and the JMP patch remain for the life of
  the target process.

- **One trampoline per vDSO entry.** Injecting a second time allocates a new page and
  re-patches the JMP (the old page leaks). This is acceptable for the testing use case.

- **No synchronisation on state reads.** The trampoline reads the state struct with plain
  word loads. A `SetTime` racing with a concurrent `clock_gettime` may observe a torn
  state and return a time between the old and new offsets. For test scenarios this is
  harmless.

- **`process_vm_writev` on the state struct requires the kernel to allow cross-process
  writes.** On kernels with strict LSM policies beyond Yama, this may fail even with
  `CAP_SYS_PTRACE`. The production agent (phase 7) can fall back to ptrace-stop + PokeText
  for the update path if needed.

---

## Project status

| Phase | Deliverable | Status |
|-------|-------------|--------|
| 0 | Project scaffolding | ✅ |
| 1 | vDSO discovery (`pkg/vdso`) | ✅ |
| 2 | ptrace primitives (`pkg/procmem`) | ✅ |
| 3 | Remote mmap (`remoteMmap` in `pkg/inject`) | ✅ |
| 4 | Trampoline assembly (`pkg/trampoline`) | ✅ |
| 5 | Injection orchestration (`pkg/inject`) | ✅ |
| 6 | Local validation harness (`faketimectl`, integration test) | ✅ |
| 7 | Node agent gRPC service (`cmd/agent`) | ✅ |
| 8 | Control-plane HTTP+JSON API (`cmd/controller`) | ✅ |
| 9 | Kubernetes manifests (`deploy/`) | ✅ |
| 10 | e2e test SDK (`pkg/sdk`) | ✅ |
| 11 | Optional TTL | ✅ |
| 12 | Dockerfiles | ✅ |
| 13 | GitHub open-source setup | ✅ |
| 14 | GitHub Actions CI | ✅ |
| 15 | Local cluster e2e (kind + Makefile) | ✅ |
| 16 | List timeshifts (`GET /timeshifts`) | ✅ |
| 17 | Health endpoint + `WithTimeT` SDK helper | ✅ |
| 18 | Handle recovery (pod/agent restarts) | ✅ |
| 19 | Prometheus metrics | ✅ |
| 20 | Controller restart recovery (ConfigMap persistence) | ✅ |
| 21 | Graceful agent shutdown (SIGTERM drain) | ✅ |
| 22 | Dry-run / resolve mode (`GET /resolve`) | ✅ |
| 23 | Agent handle status RPC (`GetStatus`) | ✅ |
| 25 | Local process injection (`pkg/faketime`, non-Kubernetes) | ✅ |
| 26 | Conflict guard (reject overlapping timeshifts, `409 Conflict`) | ✅ |
| 27 | `faketimectl` subcommand completeness (`update`, `status`) | ✅ |
| 28 | Structured logging (`log/slog`, JSON output, `LOG_LEVEL`) | ✅ |
| 29 | TTL expiry Kubernetes Events + `timeshift_expired_total` counter | ✅ |
| 30 | Lease-based leader election (`coordination.k8s.io/Lease`) | 🔲 |
| 31 | Validating webhook admission controller | 🔲 |
| 32 | `pkg/faketime` Attach path (`CAP_SYS_PTRACE`) | 🔲 |
| 33 | Integration test harness (`make test-integration`, kind) | ✅ |
| 34 | Freeze mode (pin clock at fixed instant, `--freeze` / `MaskFrozen`) | ✅ |
| 35 | Advance-by-duration (`PATCH duration`, `advance --by`, `AdvanceTimeshift`, `Handle.Advance`) | ✅ |
| 36 | Offset-based timeshift storage (live `time` in GET responses) | ✅ |
| 37 | proto `freeze` field on `InjectRequest` / `SetTimeRequest` | ✅ |
| 38 | `pkg/faketime`: `Handle.EffectiveTime()`, `Handle.PID()`, `Handle.IsAlive()`, `Session.Close()` | ✅ |
| 39 | `pkg/faketime`: `StartWithTracking` / `ChildTracker` — auto-inject into forked child processes via `PTRACE_O_TRACEFORK` | ✅ |
| 40 | `pkg/faketime`: exec-survivor injection — re-inject after `exec()` via `PTRACE_O_TRACEEXEC` so processes that self-exec (e.g. PEX bootstrap) or fork+exec retain fake time | ✅ |
| 42 | `gettimeofday`/`time` vDSO interception (previously only `clock_gettime` was patched, silently missing e.g. PostgreSQL's `GetCurrentTimestamp()`); `CLOCK_REALTIME_COARSE` support; `time()` precision fix (was up to 1s off vs. the other two) | ✅ |

See `plan.md` for the detailed specification of all phases.
See `FUTURE.md` for longer-horizon improvements (auth, multi-arch, Helm, HA).
