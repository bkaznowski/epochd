# Implementation plan: Go-based time-injection agent for Kubernetes e2e testing

Scope: code-injection approach (patch the vDSO `clock_gettime` entry point to jump into
injected code), not the ptrace-syscall-interceptor approach. x86-64 only. Pure Go plus a
small hand-assembled machine-code payload. Target use case: an e2e test sets an absolute
fake wall-clock time on specific pods via an HTTP/gRPC API — time then flows forward
normally from that point at the same rate as the real clock, with cheap (~microsecond)
updates after the first injection.

API design note: users never supply a duration/offset. They supply a target timestamp
("set this pod's clock to 2030-01-01T00:00:00Z"). Internally, the only thing that's ever
written into the target process is still an offset (`fakeTime - realTime`), since that's
what lets the clock keep ticking forward naturally rather than freezing — but that
conversion from "absolute time" to "offset" happens transparently at the lowest layer,
right before the write, never in a way the caller has to think about. This is the one
change from the original plan; it touches phases 5-10 and leaves phases 1-4 (the
injection mechanics and the assembly payload) completely untouched.

Each phase below is written as a self-contained task you can paste to a coding agent. Build
and validate phases 1-6 locally (no Kubernetes needed) before touching phases 7-10.

---

## Phase 0 — Project scaffolding

**Deliverable**: a Go module with this layout, compiling with empty/stub implementations.

```
faketime/
  go.mod
  cmd/
    agent/main.go          # node-level privileged daemon (phase 7)
    controller/main.go     # control-plane API service (phase 8)
    faketimectl/main.go    # manual CLI for local testing (phase 6)
  pkg/
    procmem/                # ptrace + process_vm_readv/writev primitives (phase 2)
    vdso/                   # vDSO discovery + ELF symbol resolution (phase 1)
    trampoline/             # hand-assembled payload + state struct (phase 4)
    inject/                 # injection orchestration (phases 3 + 5)
    api/                    # shared request/response types (phase 7/8)
    k8sresolve/             # pod -> node/containerID -> PID resolution (phase 8)
  test/
    targets/
      clockprinter/main.go  # sample injection target (phase 6)
  deploy/
    daemonset.yaml
    rbac.yaml
    controller-deployment.yaml
```

Dependencies to pull in now: `golang.org/x/sys/unix` (ptrace, process_vm_readv/writev),
`debug/elf` from the standard library (no external dep needed for ELF parsing).
Hold off on `client-go`, gRPC, etc. until phases 7-8.

---

## Phase 1 — vDSO discovery and symbol resolution (`pkg/vdso`)

**Goal**: given a PID, find the absolute address of `clock_gettime` inside that process's
vDSO mapping.

**Implement**:
```go
type VDSOInfo struct {
    Start, End      uintptr
    ClockGettimeAddr uintptr
}
func Locate(pid int) (*VDSOInfo, error)
```

Steps inside `Locate`:
1. Parse `/proc/<pid>/maps`, find the row whose pathname is `[vdso]`, extract start/end
   addresses.
2. Read the raw bytes of that mapping (via `/proc/<pid>/mem`, seeking to `Start` and
   reading `End-Start` bytes — note this requires either running as root or already
   holding a ptrace attachment on the PID, so this function should be called *after*
   attach in the real flow; for the standalone test below it's fine since it's your own
   process).
3. Wrap the bytes in `bytes.NewReader` and parse with `debug/elf.NewFile`.
4. Look up the dynamic symbol table (`.DynamicSymbols()`) for `clock_gettime` (fall back to
   `__vdso_clock_gettime` if not found — both names commonly exist).
5. Compute `absoluteAddr = Start + symbol.Value`. Sanity-check it falls within
   `[Start, End)` — if not, return an error rather than silently continuing.

**Validation** (do this before moving on): write `pkg/vdso/locate_test.go` that calls
`Locate(os.Getpid())` and prints the resolved address. Cross-check it by hand: run
`cat /proc/self/maps | grep vdso` and `objdump -T` against a dumped copy of the vDSO page
to confirm the symbol offset matches.

---

## Phase 2 — ptrace primitives (`pkg/procmem`)

**Goal**: a small, safe wrapper around ptrace that respects Go's OS-thread affinity
requirement (all ptrace calls for a given tracee must come from the same OS thread that
issued `PTRACE_ATTACH`).

**Implement**:
```go
type Tracer struct { /* owns a goroutine pinned via runtime.LockOSThread */ }

func NewTracer() *Tracer
func (t *Tracer) Attach(pid int) error          // PTRACE_ATTACH + wait for stop
func (t *Tracer) Detach() error                 // PTRACE_DETACH
func (t *Tracer) GetRegs() (*unix.PtraceRegs, error)
func (t *Tracer) SetRegs(r *unix.PtraceRegs) error
func (t *Tracer) SingleStep() error
func (t *Tracer) Cont(sig int) error
func (t *Tracer) Wait() (status unix.WaitStatus, err error)

// Bulk memory IO — works without an active ptrace-stop, only needs ptrace permission.
func ReadMem(pid int, addr uintptr, buf []byte) (int, error)   // process_vm_readv
func WriteMem(pid int, addr uintptr, buf []byte) (int, error)  // process_vm_writev

// Fallback writer for pages that reject process_vm_writev due to write-protection
// (some kernels won't let process_vm_writev hit a read-only-but-executable page even
// under ptrace permission). Uses PTRACE_POKETEXT word-by-word, which is allowed to
// write read-only pages under an active ptrace attachment (this is how debuggers set
// breakpoints in .text).
func (t *Tracer) PokeText(addr uintptr, buf []byte) error
```

Design note for the `Tracer` internals: spawn a dedicated goroutine at construction time
that calls `runtime.LockOSThread()` immediately and never unlocks it; all the exported
methods send a closure over a channel to that goroutine and block on a response channel.
This keeps every ptrace syscall pinned to one OS thread without leaking that constraint
into callers.

**Validation**: spawn a child with `exec.Command` and `SysProcAttr{Ptrace: true}` (Go's
standard mechanism — the child stops itself with `PTRACE_TRACEME` before exec). Attach,
read a known region of its memory (e.g. its own argv string visible on the stack), assert
it matches, then write a single byte and read it back to confirm the write took effect.
Detach and confirm the child resumes and exits normally.

---

## Phase 3 — Remote scratch memory via syscall injection (`pkg/inject`, `remoteMmap`)

**Goal**: make the *target* process execute `mmap(NULL, 4096, PROT_READ|PROT_WRITE|PROT_EXEC,
MAP_PRIVATE|MAP_ANONYMOUS, -1, 0)` on its own behalf, so you get back a fresh
read-write-execute page inside the target to hold your injected code. This is necessary
because the existing vDSO page has no spare room for your payload — you can only patch a
few bytes of its existing `clock_gettime` entry, not append new code to it.

**Design — reuse the patch site as the temporary syscall trampoline**: rather than hunting
for some other already-executable address to host a temporary `syscall` instruction, reuse
the vDSO `clock_gettime` address you're about to patch anyway:

1. `GetRegs()`, keep a full copy (`origRegs`) — this is what you'll restore at the very end.
2. Read and save the original 8 bytes at `ClockGettimeAddr` (`ReadMem`).
3. `PokeText` 3 bytes at `ClockGettimeAddr`: `0F 05` (`syscall`) followed by `CC` (`int3`).
4. Build a register set for the mmap call: `RIP = ClockGettimeAddr`, `RAX = 9` (`SYS_mmap`),
   `RDI = 0`, `RSI = 4096`, `RDX = 7` (`PROT_READ|WRITE|EXEC`), `R10 = 0x22`
   (`MAP_PRIVATE|MAP_ANONYMOUS`), `R8 = 0xffffffffffffffff` (`fd = -1`), `R9 = 0`.
5. `SetRegs`, then `Cont`, then `Wait` — the `int3` you planted fires immediately after the
   syscall completes, stopping the tracee again.
6. `GetRegs()` again — `RAX` now holds the new page's address. Save it.
7. Restore the original 8 bytes you saved in step 2 (undo the temporary syscall/int3 — the
   real, final patch happens in phase 5, after the payload is written).
8. Restore `origRegs` exactly (full register set, not just `RIP`).

**Validation**: attach to a spawned test child, call `remoteMmap`, then check
`/proc/<pid>/maps` for a new anonymous `rwx` 4096-byte region at the returned address. Then
detach and confirm the child is still alive and behaving normally (e.g. it's a loop
incrementing a counter you can read back out of its memory before and after).

---

## Phase 4 — Trampoline payload (`pkg/trampoline`)

**Goal**: a small, hand-assembled, position-independent x86-64 payload plus an adjacent
mutable state struct, assembled offline and embedded into the Go binary as bytes (don't
generate machine code at runtime — write it as a `.s` file, assemble it once with `nasm` or
`as`, extract the flat binary, and `//go:embed` it).

**State struct layout** (document this as a contract between the assembly and the Go side —
add a unit test that decodes a freshly-injected struct and checks the fields land at these
offsets):

```
offset  0: int64  offsetSec
offset  8: int64  offsetNsec
offset 16: uint64 enabledMask   // bit 0 = CLOCK_REALTIME
offset 24: uint32 generation    // bumped on each update, for observability
offset 28: (4 bytes padding)
```

**Trampoline logic** (write this as `pkg/trampoline/trampoline.s`, calling convention
matches `clock_gettime(int clk_id, struct timespec *tp)`: `clk_id` in `RDI`, `tp` in `RSI`
— this happens to match the raw Linux syscall ABI for the first two args too, so no
register shuffling is needed before issuing the syscall):

```asm
; entry: rdi = clk_id, rsi = struct timespec*
push rdi
push rsi
mov  rax, 228        ; SYS_clock_gettime — get the real time first, always
syscall
pop  rsi
pop  rdi
cmp  rdi, 0          ; CLOCK_REALTIME == 0 — only this clock is faked
jne  .done
lea  r11, [rip + state]   ; RIP-relative: works regardless of where this page lands
mov  r8,  [r11]            ; offsetSec
mov  r9,  [r11 + 8]        ; offsetNsec
add  [rsi],     r8         ; ts->tv_sec  += offsetSec
add  [rsi + 8], r9         ; ts->tv_nsec += offsetNsec
; normalize tv_nsec into [0, 1e9)
mov  rax, [rsi + 8]
cmp  rax, 1000000000
jl   .check_negative
sub  rax, 1000000000
mov  [rsi + 8], rax
inc  qword [rsi]
jmp  .done
.check_negative:
cmp  rax, 0
jge  .done
add  rax, 1000000000
mov  [rsi + 8], rax
dec  qword [rsi]
.done:
xor  rax, rax
ret
state:
dq 0     ; offsetSec
dq 0     ; offsetNsec
dq 1     ; enabledMask (CLOCK_REALTIME on by default)
dd 0     ; generation
dd 0     ; padding
```

Key design point worth calling out explicitly to whoever implements this: the trampoline
never tries to call back into the original (now partially overwritten) vDSO function — it
just issues the raw syscall itself to get the real time, then adds the offset. This avoids
needing to save/relocate/re-execute the original instruction bytes, which is the fiddly
part of classic inline-hook implementations and isn't necessary here.

Assemble with `nasm -f bin trampoline.s -o trampoline.bin`, embed with
`//go:embed trampoline.bin` into a `[]byte` constant, and hardcode a Go constant
`stateOffset` equal to the byte offset of the `state:` label (count it once after
assembling — `objdump -D -b binary -m i386:x86-64 trampoline.bin` will show you exactly
where it lands). Add a regression test that re-decodes the embedded bytes and asserts the
`enabledMask` field at `stateOffset+16` reads as `1`, so a future edit to the assembly that
shifts the struct without updating `stateOffset` fails CI loudly instead of corrupting
memory silently at runtime.

---

## Phase 5 — Injection orchestration (`pkg/inject`)

**Goal**: tie phases 1-4 together into the public API the rest of the system uses.

```go
type Handle struct {
    PID       int
    StateAddr uintptr
}

// Low-level primitives — match the wire format of the injected struct exactly.
func Inject(pid int, initialOffsetSec, initialOffsetNsec int64) (*Handle, error)
func (h *Handle) SetOffset(offsetSec, offsetNsec int64) error  // process_vm_writev only

// Public, time-based wrappers — this is what callers above pkg/inject should use.
// They compute "offset = target - now" right before the underlying call, so the
// conversion happens as close as possible to the actual write.
func InjectAtTime(pid int, target time.Time) (*Handle, error) {
    now := time.Now()
    sec, nsec := diffSecNsec(target, now)
    return Inject(pid, sec, nsec)
}

func (h *Handle) SetTime(target time.Time) error {
    now := time.Now()
    sec, nsec := diffSecNsec(target, now)
    return h.SetOffset(sec, nsec)
}
```

`Inject` sequence: `vdso.Locate` -> `Tracer.Attach` -> `remoteMmap` (phase 3) -> write the
trampoline+struct blob into the new page with offsets pre-set to the caller's initial
values -> compute the `jmp rel32` (`0xE9` followed by a little-endian 4-byte displacement
= `newPageAddr - (ClockGettimeAddr + 5)`) -> `PokeText` it over the vDSO entry's first 5
bytes -> `Tracer.Detach`. Save the original 5 bytes you overwrote on the `Handle` too, even
if you don't implement full uninstall in v1 — you'll want it later.

`SetOffset` is deliberately decoupled from the `Tracer`/ptrace machinery entirely — it's
just `procmem.WriteMem(h.PID, h.StateAddr, encodedBytes)`, which is the whole point: after
the one-time injection, updates are a single syscall with no attach/detach overhead.

Keep `Inject`/`SetOffset` as the primitives every layer above ultimately calls — they're
what actually matches the assembly's struct layout from phase 4 — but make `InjectAtTime`
and `SetTime` the only entry points exported out of this package's public surface that
the agent (phase 7) actually calls. There's no reason for "offset" to leak any further up
the stack than this file.

---

## Phase 6 — Local validation harness (no Kubernetes yet)

**Build** `test/targets/clockprinter/main.go`: an infinite loop that prints
`time.Now().Format(time.RFC3339)` once per second. This forces Go's runtime to repeatedly
call `clock_gettime` through the vDSO — a faithful stand-in for the real target processes.

**Build** `cmd/faketimectl/main.go`: a CLI taking `--pid` and `--set-time` (RFC3339
timestamp, e.g. `2030-01-01T00:00:00Z`), that calls `inject.InjectAtTime` or
`(*Handle).SetTime` for manual testing. A `--reset` flag should call `SetTime(time.Now())`
to snap the target back to the real clock.

**Integration test**: spawn `clockprinter` as a subprocess, capture stdout, call
`InjectAtTime(pid, time.Now().Add(24*time.Hour))`, assert the next several printed
timestamps start at ~24h ahead of wall clock and keep advancing at the normal rate from
there, call `SetTime(time.Now())`, assert timestamps return to real time, then kill the
subprocess. Get this fully green before writing any Kubernetes-facing code — it's the
entire hard part validated in isolation.

---

## Phase 7 — Node agent service (`cmd/agent`)

**Goal**: wrap the injector behind a small API a control plane can call, plus container ID
to PID resolution.

- Connect to the local container runtime's CRI socket (containerd:
  `/run/containerd/containerd.sock`) using `k8s.io/cri-api`'s generated gRPC client, call
  `ContainerStatus` to map a container ID to its init PID.
- Expose a small gRPC (or HTTP+JSON for a faster first pass) service:
  `Inject(containerID, targetTimeUnixNano) -> handleID`,
  `SetTime(handleID, targetTimeUnixNano) -> ok`,
  `Status(handleID) -> currentOffset, lastSetTime`.
- Keep an in-memory map of `handleID -> *inject.Handle` for the life of the agent process.

Do the absolute-time-to-offset conversion (`inject.InjectAtTime` / `(*Handle).SetTime`)
right here in the agent, not in the controller — the agent is the last hop before the
actual memory write, so this is where `time.Now()` is most accurate relative to when the
write actually lands. If the conversion happened in the controller instead, network
latency between controller and agent (which can be tens to hundreds of milliseconds,
especially under load) would leak into the target's perceived time as drift away from the
timestamp the caller actually asked for.

This runs as a privileged DaemonSet pod (`hostPID: true`, capability `SYS_PTRACE`, the CRI
socket mounted in) — see phase 9 for the manifest.

---

## Phase 8 — Control plane (`cmd/controller`)

**Goal**: the public-facing API from the architecture discussion earlier:

```
POST   /skews    { namespace, labelSelector, time, ttl } -> { id, appliedTo[] }
PATCH  /skews/{id}  { time }
DELETE /skews/{id}
GET    /skews/{id}    -> { id, time, appliedTo[], expiresAt }
```

`time` is an RFC3339 timestamp throughout — "set these pods' clocks to this instant, then
let time run forward normally." There's no offset/duration field anywhere in this API; the
controller just passes the timestamp straight through to each node agent unchanged (see
phase 7 for why the time-to-offset conversion happens there, not here).

- Use `client-go` to resolve `labelSelector` to a list of pods, pulling
  `.status.hostIP` and `.status.containerStatuses[].containerID` for each.
- Maintain a registry mapping node IP -> agent endpoint (the DaemonSet pod on that node,
  reachable directly by pod IP since you already know which node each target pod is on).
- For each target, call the node agent's `Inject` (first time) or `SetTime` (subsequent
  `PATCH` calls, using the handle ID you cached from the first call).
- Background goroutine sweeping expired TTLs and issuing `SetTime(time.Now())` on each
  affected handle automatically, snapping the target back to the real clock.

---

## Phase 9 — Kubernetes manifests (`deploy/`)

- `daemonset.yaml`: the agent, `hostPID: true`, `securityContext.capabilities.add:
  ["SYS_PTRACE"]`, CRI socket mounted via `hostPath`.
- `rbac.yaml`: a ServiceAccount for the controller with `get/list/watch` on `pods`.
- `controller-deployment.yaml`: the controller as a normal (unprivileged) Deployment plus a
  `ClusterIP` Service, so test code in-cluster (or port-forwarded) can reach
  `faketime-controller.faketime-system.svc`.

Note for whoever deploys this: the agent's privilege level (`SYS_PTRACE` + `hostPID`) means
anything that can reach its API can skew the clock of any process on that node. Put the
controller's API behind cluster-internal-only access (no public Service/Ingress) and
restrict who can reach it via NetworkPolicy, since this isn't something you want exposed
beyond your test infrastructure.

---

## Phase 10 — e2e test SDK

A thin client library wrapping the controller's HTTP API with a scope-bound helper, e.g.
in Go:

```go
func WithTime(t *testing.T, selector string, target time.Time, fn func()) {
    id := createSkew(selector, target, defaultTTL)
    defer deleteSkew(id) // runs even if fn panics or the assertion fails
    fn()
}
```

so a test reads as `faketime.WithTime(t, "app=order-service",
time.Now().AddDate(0, 0, 30), func() { ...assertions... })` and never needs to think about
cleanup — or about converting a "30 days from now" mental model into a duration the API
doesn't actually want.

---

## Suggested order to actually ask Claude to implement these

Phases 1, 2, and 6's `clockprinter` target can be built and tested in any order, then phase
3, then 4 (the assembly can be written and unit-tested independently of 1-3), then 5 ties
everything together and is where you'll do most of your real debugging, then 6's
integration test is the milestone that proves the core idea works end to end. Only move to
7-10 once 6 is reliably green — they're comparatively mechanical (CRI lookups, REST
handlers, client-go watches) once the injection core is solid.

---

## Phase 11 — Optional TTL

**Goal**: allow timeshifts to be created without a TTL so they persist until explicitly
deleted. This is useful when the test duration is unknown up-front or when you want a
timeshift to outlive a single test function.

**API change**: `ttl` in `CreateTimeshiftRequest` becomes optional. An empty string (or
omitted field) means "no expiry". `expiresAt` in `TimeshiftResponse` is likewise optional —
omitted (empty string) when no TTL was set.

**Changes required**:

1. **`pkg/api/api.go`** — add `omitempty` to the `TTL` and `ExpiresAt` json tags so the
   wire format is clean rather than sending `"ttl":""`:
   ```go
   TTL       string `json:"ttl,omitempty"`
   ExpiresAt string `json:"expiresAt,omitempty"`
   ```

2. **`cmd/controller/controller.go`** — the `skew` struct's `ttl` and `expiresAt` fields
   remain as-is; a zero `expiresAt` (`time.Time{}`) means "never expires".
   - `createTimeshift`: only set `expiresAt` and `ttl` when the caller supplied a non-zero
     TTL; leave both at their zero values otherwise.
   - `sweepExpired`: add a guard — skip any timeshift whose `expiresAt.IsZero()`.
   - `toResponse`: only populate `ExpiresAt` in the response when `expiresAt` is non-zero.

3. **`cmd/controller/handlers.go`** — relax the TTL validation: a missing/empty TTL is now
   valid. Only reject explicitly invalid values (e.g. negative durations or unparseable
   strings that aren't empty).

4. **`pkg/sdk/sdk.go`** — `CreateTimeshift` accepts `ttl time.Duration`; a zero value means
   no TTL. The `do` helper already marshals via `api.CreateTimeshiftRequest`, so a zero TTL
   maps to `""` with `omitempty` and the field is omitted from the JSON body.
   `WithTime` should continue to require a non-zero TTL as a safety guard — it is a
   scoped helper that is expected to clean up, and an infinite-TTL `WithTime` is almost
   certainly a mistake. Document this constraint in the function's godoc.

5. **Tests** — add cases to `controller_test.go` and `sdk_test.go`:
   - Create a timeshift with no TTL; confirm it is not swept after its notional expiry.
   - Confirm `ExpiresAt` is absent from the JSON response when no TTL is set.
   - Confirm the timeshift is still retrievable until explicitly deleted.

---

## Phase 12 — Dockerfiles

**Goal**: produce two minimal Docker images — one per binary — that can be pushed to a
registry and loaded into a Kubernetes cluster.

**`Dockerfile.agent`** (Linux-only; must be built with `--platform linux/amd64`):
```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /agent ./cmd/agent

FROM scratch
COPY --from=build /agent /agent
ENTRYPOINT ["/agent"]
```

**`Dockerfile.controller`** (no OS restriction at build time; runs on Linux in the cluster):
```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /controller ./cmd/controller

FROM scratch
COPY --from=build /controller /controller
ENTRYPOINT ["/controller"]
```

Both images use `scratch` as the runtime base — the binaries have no libc dependency
(CGO_ENABLED=0, pure-Go net resolver). This keeps each image under ~20 MB.

**Image names** — use `ghcr.io/<github-org>/epochd-agent:<tag>` and
`ghcr.io/<github-org>/epochd-controller:<tag>`. Update `deploy/daemonset.yaml` and
`deploy/controller-deployment.yaml` with the final registry paths once the org is known.

**Validation**: `docker build -f Dockerfile.agent --platform linux/amd64 -t epochd-agent:dev .`
and `docker run --rm epochd-agent:dev --help` should print the agent's usage without error.
Repeat for the controller image.

---

## Phase 13 — GitHub repository and open-source setup

**Goal**: prepare the repository for public visibility on GitHub.

1. **License** — add `LICENSE` at the repo root. Apache 2.0 is the conventional choice for
   infrastructure tooling (compatible with most corporate open-source policies); MIT is a
   simpler alternative. Choose one and commit it.

2. **`.gitignore`** — standard Go gitignore: binaries (`/agent`, `/controller`,
   `/faketimectl`), test caches (`/tmp/`, `*.test`), IDE files. The generated protobuf files
   in `pkg/agentpb/` should be committed (they're checked-in generated code, not build
   artefacts).

3. **`go.sum`** — must be committed. It is the module's reproducibility proof and is
   expected by `go mod verify` in CI.

4. **README polish** — add a badge row at the top once the GitHub Actions workflow exists
   (CI status, Go version, license). Confirm the "how to run" instructions reference the
   correct binary names and module path (`epochd`, not `faketime`).

5. **Repository settings** — enable branch protection on `main`: require the CI workflow to
   pass before merge, require at least one approving review if the project will accept
   external contributions.

---

## Phase 14 — GitHub Actions CI

**Goal**: automated checks on every push and pull request.

**`.github/workflows/ci.yml`** — three jobs, all running on `ubuntu-latest`:

```
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - run: go test ./...
      # Note: agent and inject tests carry //go:build linux and run fine here.
      # The controller, SDK, and agentclient tests have no build tag.

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - uses: golangci/golangci-lint-action@v6
        with:
          version: latest
          args: --timeout=5m

  build-images:
    runs-on: ubuntu-latest
    permissions:
      packages: write   # needed to push to ghcr.io
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v5
        with:
          file: Dockerfile.agent
          platforms: linux/amd64
          push: ${{ github.ref == 'refs/heads/main' }}
          tags: ghcr.io/${{ github.repository_owner }}/epochd-agent:latest
      - uses: docker/build-push-action@v5
        with:
          file: Dockerfile.controller
          platforms: linux/amd64
          push: ${{ github.ref == 'refs/heads/main' }}
          tags: ghcr.io/${{ github.repository_owner }}/epochd-controller:latest
```

Images are only pushed on merges to `main`; PRs only build (no push) to validate the
Dockerfiles don't regress.

A minimal **`.golangci.yml`** at the repo root keeps lint from being noisy:
```yaml
linters:
  enable:
    - errcheck
    - staticcheck
    - unused
    - govet
linters-settings:
  errcheck:
    exclude-functions:
      - (net/http.ResponseWriter).Write   # intentional fire-and-forget in handlers
```

---

## Phase 15 — Local cluster e2e with kind

**Goal**: a single `make e2e` command that spins up a local Kubernetes cluster, deploys
epochd, and runs an end-to-end test that proves the clock injection works in a real cluster.

**`Makefile`** targets:

```makefile
CLUSTER   = epochd-dev
IMAGE_TAG = dev

cluster:
	kind create cluster --name $(CLUSTER)

delete-cluster:
	kind delete cluster --name $(CLUSTER)

images:
	docker build -f Dockerfile.agent     --platform linux/amd64 -t epochd-agent:$(IMAGE_TAG) .
	docker build -f Dockerfile.controller --platform linux/amd64 -t epochd-controller:$(IMAGE_TAG) .

load: images
	kind load docker-image epochd-agent:$(IMAGE_TAG)      --name $(CLUSTER)
	kind load docker-image epochd-controller:$(IMAGE_TAG) --name $(CLUSTER)

deploy: load
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/daemonset.yaml
	kubectl apply -f deploy/controller-deployment.yaml
	kubectl rollout status daemonset/epochd-agent      -n epochd --timeout=60s
	kubectl rollout status deployment/epochd-controller -n epochd --timeout=60s

e2e: deploy
	kubectl port-forward svc/epochd-controller 18080:80 -n epochd &
	sleep 2
	EPOCHD_URL=http://localhost:18080 go test ./e2e/... -v -timeout=2m
	pkill -f "port-forward svc/epochd-controller" || true
```

**`e2e/e2e_test.go`** — the end-to-end test:

```go
//go:build e2e

package e2e_test

// TestTimeshiftDate deploys a sleep pod, shifts its clock 365 days forward, execs
// `date +%Y` into it, and asserts the year matches.
func TestTimeshiftDate(t *testing.T) {
    controllerURL := os.Getenv("EPOCHD_URL")
    if controllerURL == "" {
        t.Skip("EPOCHD_URL not set")
    }

    // deploy a simple test pod (busybox sleep)
    // create timeshift via SDK
    // exec `date +%Y` into the pod
    // assert year == time.Now().AddDate(1,0,0).Year()
    // SDK WithTime cleans up the timeshift on return
}
```

The `//go:build e2e` tag keeps the e2e tests out of `go test ./...` in CI; the Makefile
passes `-tags e2e` explicitly. This means CI (phase 14) runs unit tests only, while local
development and a separate scheduled e2e job can run the full suite.

**Dependency**: `kind` must be installed locally (`go install sigs.k8s.io/kind@latest` or
via the package manager). The Makefile targets assume `kubectl` and `docker` are also
available. Add a `check-deps` target that verifies these are present and prints a clear
error if not.

---

## Phase 16 — List timeshifts (`GET /timeshifts`)

**Goal**: expose all active timeshifts so operators and tests can inspect the current state
of the controller without knowing individual IDs.

**Changes required**:

1. **`pkg/api/api.go`** — add a response type:
   ```go
   type ListTimeshiftsResponse struct {
       Timeshifts []TimeshiftResponse `json:"timeshifts"`
   }
   ```

2. **`cmd/controller/controller.go`** — add a `listTimeshifts` method that acquires a read
   lock, copies all values from the `timeshifts` map into a slice, and returns it sorted by
   `createdAt` (oldest first) so the output is stable across calls:
   ```go
   func (c *controller) listTimeshifts() []api.TimeshiftResponse
   ```

3. **`cmd/controller/handlers.go`** — register `"GET /timeshifts"` and implement
   `handleListTimeshifts`, which calls `c.listTimeshifts()` and writes a
   `ListTimeshiftsResponse` with HTTP 200.

4. **`pkg/sdk/sdk.go`** — add `ListTimeshifts(ctx) ([]Timeshift, error)` that calls
   `GET /timeshifts` and parses the response into a `[]Timeshift`.

5. **Tests** — add to `controller_test.go`: create two timeshifts, call `listTimeshifts`,
   assert both appear in the result sorted by creation time. Add to `sdk_test.go`: extend
   the fake server to handle `GET /timeshifts` and add a `TestListTimeshifts` case.

---

## Phase 17 — Health endpoint and `WithTimeT` SDK helper

**Goal**: give the controller a real health endpoint and give test authors a more ergonomic
SDK entry point that integrates with `*testing.T`.

**`/healthz` endpoint** (`cmd/controller/handlers.go` and `cmd/controller/controller.go`):

Register `"GET /healthz"` returning HTTP 200 with body `{"status":"ok"}`. The handler can
optionally perform a lightweight self-check — e.g. confirm the k8s client can reach the
API server with a short-timeout `Discovery().ServerVersion()` call — and return HTTP 503 if
it fails. Update `deploy/controller-deployment.yaml` to use an HTTP `httpGet` readiness
probe on `/healthz` instead of the current TCP socket check.

**`WithTimeT` helper** (`pkg/sdk/sdk.go`):

A variant of `WithTime` that accepts `*testing.T` and wires up cleanup and skip logic
automatically:

```go
// WithTimeT creates a timeshift, calls fn, and registers a t.Cleanup to delete
// it. It skips the test automatically if EPOCHD_URL is not set, making it safe
// to call unconditionally in any test file — tests just skip in environments
// where epochd is not deployed.
//
// ttl must be positive. Use CreateTimeshift directly for no-expiry timeshifts.
func WithTimeT(
    t *testing.T,
    c *Client,
    ns, labelSelector string,
    target time.Time,
    ttl time.Duration,
    fn func(t *testing.T, ts *Timeshift),
)
```

Key behaviours:
- Calls `t.Helper()` so failures point at the caller, not the helper.
- Registers the delete via `t.Cleanup` (runs even on `t.Fatal`; `defer` inside a subtest
  does not).
- Calls `t.Fatalf` on create failure rather than returning an error, since a failed create
  means the test cannot proceed.

**Tests** — add `TestWithTimeT` to `sdk_test.go` using `httptest.NewServer`.

---

## Phase 18 — Handle recovery

**Goal**: make the controller self-heal when handles become stale — either because a pod
restarted (new container ID, new PID) or because the agent itself restarted (all in-memory
handles lost).

There are two distinct failure modes:

### 18a — Agent restart (handles lost)

When the agent restarts, `SetTime` and `Reset` RPCs for existing handles return
`codes.NOT_FOUND`. The controller should detect this and re-inject.

- In `updateTimeshift` and `resetHandles`, inspect errors from the agent. When a
  `codes.NOT_FOUND` gRPC status is received, attempt `Inject` on the same `containerID`
  with the timeshift's current `targetTime`, update the stored `agentHandle` with the new
  handle ID, and retry the original operation. Log a warning for observability.
- Add `containerID` to `containerHandle` so the controller has what it needs to re-inject
  without a new pod list call.

### 18b — Pod restart (container gone)

When a container restarts, the agent's `Inject` call (during re-injection above) will
return `codes.NOT_FOUND` from `k8sresolve.LookupPID` — the container ID no longer exists.
At that point the handle is unrecoverable until the next `createTimeshift` call selects the
new container.

The controller should watch pod events via `client-go` informers and re-run `injectPod`
when it sees a pod whose label selector matches an active timeshift transition from
non-Running to Running. This ensures new container instances are injected automatically
without user intervention.

**Suggested approach for the pod watcher**:

```go
// In controller, started alongside the sweeper:
func (c *controller) startPodWatcher(ctx context.Context) {
    // Use a SharedInformerFactory with a label selector that matches
    // any of the active timeshift selectors. On pod Running transition,
    // find matching timeshifts and call injectPod for the new container.
}
```

Note: the pod watcher adds a dependency on `k8s.io/client-go/informers`. The controller
already imports `client-go`, so no new module dependency is needed.

**Tests** — unit-test the re-injection path in `controller_test.go` by having the mock
`AgentPool.SetTime` return `codes.NOT_FOUND` for the first call and succeed on the second,
then asserting the `agentHandle` was updated.

---

## Phase 19 — Prometheus metrics

**Goal**: expose key operational metrics so the controller can be monitored with standard
Kubernetes tooling.

**Dependency**: add `github.com/prometheus/client_golang` to `go.mod`.

**Metrics to expose** (all registered in a `newMetrics()` constructor and injected into
`controller` at construction time):

| Metric | Type | Description |
|--------|------|-------------|
| `epochd_timeshifts_active` | Gauge | Number of timeshifts currently in the registry |
| `epochd_inject_total` | Counter | Injections attempted, labelled `result=success\|error` |
| `epochd_settime_total` | Counter | SetTime calls, labelled `result=success\|error` |
| `epochd_sweep_expired_total` | Counter | Timeshifts removed by the TTL sweeper |
| `epochd_api_requests_total` | Counter | HTTP requests, labelled `method`, `path`, `status` |

**Wiring**:

- Register a `"GET /metrics"` route in `routes()` that serves
  `promhttp.Handler()` from `prometheus/client_golang/prometheus/promhttp`.
- Increment counters at the call sites in `controller.go` and the HTTP handlers.
- Update the Gauge in `createTimeshift`, `deleteTimeshift`, and `sweepExpired`.

**`deploy/controller-deployment.yaml`** — add a Prometheus scrape annotation so
`kube-prometheus-stack` (or any standard scrape config) picks it up automatically:
```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port:   "8080"
  prometheus.io/path:   "/metrics"
```

**Tests** — add a test that creates and deletes a timeshift via the controller, then GETs
`/metrics` and asserts the relevant counter values are present in the response body. No
Prometheus client needed in the test — a plain string search on the text exposition format
is sufficient.

---

## Phase 20 — Controller restart recovery

**Goal**: survive controller pod restarts without leaving injected containers permanently
stuck on their fake clocks. Currently, a controller eviction or upgrade loses all
in-memory timeshift state — the TTL sweeper can no longer clean up, `GET /timeshifts`
returns empty, and updates fail — even though the trampoline is still live in every
injected container.

**Approach**: persist the timeshift registry to a Kubernetes ConfigMap on every write and
reload it on startup. A ConfigMap is sufficient (no etcd, no leader election): the data is
small, writes are infrequent, and a single-replica controller is the normal deployment.

**Changes required**:

1. **`cmd/controller/store.go`** (new) — a `store` type that wraps a ConfigMap:
   ```go
   type store struct {
       k8s       kubernetes.Interface
       namespace string
       name      string  // e.g. "epochd-state"
   }

   // save serialises the timeshifts map to JSON and writes it to the ConfigMap's
   // "state" key. Called under the controller's write lock.
   func (s *store) save(ctx context.Context, timeshifts map[string]*timeshift) error

   // load reads the ConfigMap and deserialises the timeshifts map. Called once at
   // startup before serving requests.
   func (s *store) load(ctx context.Context) (map[string]*timeshift, error)
   ```
   Use `CreateOrUpdate` (get + patch) rather than a blind create so restarts are
   idempotent. The ConfigMap may not exist yet on first run — create it if absent.

2. **`cmd/controller/controller.go`** — add a `store *store` field; call `store.save`
   after every mutation (`createTimeshift`, `updateTimeshift`, `deleteTimeshift`,
   `sweepExpired`). On startup (in `newController` or a new `controller.restore` method),
   call `store.load` and populate the in-memory map before accepting requests.

3. **`cmd/controller/main.go`** — construct the store with the controller's own namespace
   (from the `CONTROLLER_NAMESPACE` env var or a `--namespace` flag) and pass it to
   `newController`.

4. **`deploy/rbac.yaml`** — extend the controller's `ClusterRole` (or add a `Role` in the
   controller's namespace) to allow `get`, `create`, `update`, `patch` on `configmaps`.

5. **`deploy/controller-deployment.yaml`** — add a `CONTROLLER_NAMESPACE` env var sourced
   from the downward API:
   ```yaml
   env:
     - name: CONTROLLER_NAMESPACE
       valueFrom:
         fieldRef:
           fieldPath: metadata.namespace
   ```

**Serialisation note**: the `containerHandle` fields (`pod`, `container`, `nodeIP`,
`containerID`, `agentHandle`) must all be exported or the struct must have explicit JSON
tags for encoding/decoding to work correctly. Add `json:"..."` tags to both
`containerHandle` and `timeshift`.

**Tests** — unit-test `store.save` + `store.load` using the fake k8s client already used
in `controller_test.go`. Verify a round-trip: save a registry, load it back, assert the
timeshifts are identical. Add a controller-level test that saves state, creates a new
controller that calls `restore`, and confirms the timeshifts are present.

---

## Phase 21 — Graceful agent shutdown

**Goal**: when the agent pod is terminated (SIGTERM), reset all active handles before
exiting so injected containers are left with their real clocks rather than a permanently
drifting fake one.

Currently an agent eviction leaves every injected container running the trampoline with
a stale offset. The controller only discovers this on the next `SetTime` or `Reset` call
(which returns `NOT_FOUND`), at which point it re-injects. Between eviction and that next
call, the container's clock is wrong and silently drifting.

**Changes required**:

1. **`cmd/agent/main.go`** — replace the bare `signal.NotifyContext` with an explicit
   drain step:
   ```go
   ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
   defer stop()

   // ... serve ...

   <-ctx.Done()
   log.Printf("agent: received shutdown signal, resetting %d handles", len(handles))
   drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
   defer cancel()
   resetAll(drainCtx, handles)
   ```
   `resetAll` calls `h.SetTime(time.Now())` on every `*inject.Handle` in the map.
   Errors are logged but not fatal — best-effort cleanup on shutdown is acceptable.

2. **Handle map access** — `resetAll` needs a snapshot of all handles. Take a read lock,
   copy the values, release the lock, then reset without holding the lock (the gRPC calls
   in `SetTime` block).

3. **Shutdown timeout** — 10 seconds is generous for `process_vm_writev`-based resets
   (each is a single syscall). The Kubernetes `terminationGracePeriodSeconds` in
   `deploy/daemonset.yaml` should be set to at least 15 s to give the drain time to
   complete before the pod is force-killed.

4. **`deploy/daemonset.yaml`** — add `terminationGracePeriodSeconds: 15` to the pod spec.

**Tests** — unit-test that `resetAll` is called with the correct handles when the context
is cancelled. Since the actual `SetTime` calls go through `process_vm_writev` (Linux-only),
mock the handle's reset method in the test or test only the drain-loop logic.

---

## Phase 22 — Dry-run / resolve mode

**Goal**: let callers see which pods and containers a given namespace + label selector
would affect *without* performing any injection. This is the most common debugging step
when a selector matches unexpectedly many or few pods.

**Changes required**:

1. **`pkg/api/api.go`** — add a response type:
   ```go
   type ResolveResponse struct {
       Pods []ResolvedPod `json:"pods"`
   }
   type ResolvedPod struct {
       Name       string   `json:"name"`
       Namespace  string   `json:"namespace"`
       NodeIP     string   `json:"nodeIP"`
       Containers []string `json:"containers"` // running containers only
   }
   ```

2. **`cmd/controller/handlers.go`** — register `"GET /resolve"` and implement
   `handleResolve`:
   - Parse `namespace` and `selector` query parameters.
   - Call `c.k8s.CoreV1().Pods(ns).List(...)` with the selector.
   - For each pod, collect running container names (same filter as `injectPod`).
   - Return a `ResolveResponse` with HTTP 200. No agent calls, no state mutation.

3. **`pkg/sdk/sdk.go`** — add `Resolve(ctx, namespace, selector string) ([]ResolvedPod, error)`.

4. **Tests** — add to `controller_test.go`: create two pods, call `GET /resolve`, assert
   both pods appear with their running containers. Verify no handles are created and no
   agent calls are made.

**Also consider**: a `?dry-run=true` query parameter on `POST /timeshifts` that runs
the full resolution and returns what *would* be injected (same `ResolveResponse`) without
mutating state. This is ergonomic for callers who already construct a
`CreateTimeshiftRequest` and just want to preview it.

---

## Phase 23 — Agent handle status RPC

**Goal**: surface the current observed state of an injected container — the live targetTime
and generation counter from the trampoline's state struct — so operators can confirm
injection is working and diagnose drift.

Currently there is no way to verify from outside whether a container is actually seeing the
injected time. The generation counter already exists in the trampoline state struct
(`Handle.StateAddr`); it just needs to be read and surfaced.

**Changes required**:

1. **`proto/agent/v1/agent.proto`** — add a `GetStatus` RPC:
   ```protobuf
   message GetStatusRequest  { string handle_id = 1; }
   message GetStatusResponse {
     string handle_id    = 1;
     string target_time  = 2; // RFC3339, last value written by SetTime
     uint32 generation   = 3; // bumped on each SetTime; 0 = initial injection
     string state_addr   = 4; // hex, for debugging
   }
   rpc GetStatus(GetStatusRequest) returns (GetStatusResponse);
   ```
   Regenerate `pkg/agentpb/` after editing the proto.

2. **`cmd/agent/main.go`** — implement `GetStatus`: look up the handle by ID, call
   `procmem.ReadMem(h.PID, h.StateAddr, stateBytes)`, decode with
   `trampoline.DecodeState`, return the fields. Reading the state struct does not require
   ptrace — `process_vm_readv` is sufficient.

3. **`pkg/agentclient/agentclient.go`** — add `GetStatus(ctx, nodeIP, handleID string) (*api.HandleStatus, error)`.

4. **`AgentPool` interface** (`cmd/controller/controller.go`) — add `GetStatus`.

5. **`cmd/controller/handlers.go`** — add `GET /timeshifts/{id}/status` that calls
   `GetStatus` for every handle in the timeshift and returns an augmented response
   including per-container generation and last-seen targetTime.

6. **`pkg/sdk/sdk.go`** — add `TimeshiftStatus(ctx, id string)`.

**Tests** — unit-test the controller handler with a mock that returns a canned status.
The agent-side read from the state struct is already covered by `TestInjectMechanics`.

---

## Phase 25 — Local process time injection (`pkg/faketime`)

**Goal**: make the vDSO injection usable directly in Go tests that start their own
processes — postgres, redpanda, Go services, Python services — without Kubernetes. This
is the non-cluster path: the test binary injects fake time directly into child processes
on the same Linux host.

This phase builds a `pkg/faketime` package on top of the existing `pkg/inject`
primitives. No new injection mechanics are needed; the work is entirely in the API layer.

---

### Background: two injection paths

| Path | How | Permissions | Use when |
|------|-----|-------------|----------|
| `FollowChild` | Child calls `PTRACE_TRACEME` before exec | None beyond spawning the process | Test starts the process itself via `exec.Cmd` |
| `Attach` | Parent calls `PTRACE_ATTACH` on a running PID | `CAP_SYS_PTRACE` + `ptrace_scope ≤ 1` | Process was started externally (testcontainers, separate binary) |

For containerised dependencies (postgres, redpanda via Docker), the PID required is the
**host-namespace PID** from `docker inspect --format '{{.State.Pid}}' <container>`.
testcontainers-go exposes this via `container.Inspect(ctx).State.Pid`. This works on a
Linux host or a Linux CI runner; it does not work on macOS or Windows because Docker runs
inside a Linux VM whose PID namespace is inaccessible from the host.

---

### Single-process API

```go
// pkg/faketime/faketime.go
// //go:build linux

// Handle holds an active time injection for one process.
type Handle struct {
    h *inject.Handle
}

// Start starts cmd with fake time injected from the moment the process begins.
// It sets SysProcAttr{Ptrace: true} on cmd, calls cmd.Start(), waits for the
// initial SIGTRAP via FollowChild, injects, then detaches. The process runs
// freely after Start returns.
// No elevated permissions required.
func Start(cmd *exec.Cmd, target time.Time) (*Handle, error)

// Attach injects fake time into an already-running process.
// Requires CAP_SYS_PTRACE and /proc/sys/kernel/yama/ptrace_scope ≤ 1.
func Attach(pid int, target time.Time) (*Handle, error)

// SetTime updates the fake time without a ptrace stop (process_vm_writev only).
func (h *Handle) SetTime(target time.Time) error

// Reset snaps the process back to real time. Equivalent to SetTime(time.Now()).
func (h *Handle) Reset() error
```

`Start` must set `SysProcAttr` before `cmd.Start()`, so it takes ownership of starting
the process. The caller must not call `cmd.Start()` themselves.

---

### Multi-process Session

A `Session` groups multiple handles under a single target time and updates them
concurrently to minimise the inter-process race window.

```go
// Session manages fake time for a group of processes sharing the same target.
type Session struct { /* unexported */ }

// NewSession creates an empty session. Processes are added via Start and Attach.
func NewSession(target time.Time) *Session

// Start starts cmd with fake time and adds the resulting handle to the session.
func (s *Session) Start(cmd *exec.Cmd) error

// Attach attaches to an already-running process and adds it to the session.
func (s *Session) Attach(pid int) error

// SetTime updates the fake time for all processes in the session concurrently.
// All process_vm_writev calls are issued in parallel; the total elapsed time
// equals one write latency (~1 µs) regardless of session size. Errors from
// individual processes are collected and returned as a joined error.
func (s *Session) SetTime(target time.Time) error

// Reset snaps all processes back to real time (calls SetTime(time.Now())).
func (s *Session) Reset() error

// Len returns the number of processes currently in the session.
func (s *Session) Len() int
```

---

### Testing helpers

```go
// WithProcess is a single-process testing helper using the FollowChild path.
// It starts cmd with fake time, registers t.Cleanup to reset and wait, and
// calls fn with the active handle. No elevated permissions required.
func WithProcess(t *testing.T, cmd *exec.Cmd, target time.Time,
    fn func(t *testing.T, h *Handle))

// WithPID is a single-process testing helper using the Attach path.
// It attaches to an already-running process and registers t.Cleanup to reset.
// Requires CAP_SYS_PTRACE.
func WithPID(t *testing.T, pid int, target time.Time,
    fn func(t *testing.T, h *Handle))

// WithSession is the multi-process testing helper.
// setup is called first to add processes to the session; fn is called once all
// additions succeed. t.Cleanup resets all processes and waits on any cmds
// added via session.Start.
//
// Example:
//
//   faketime.WithSession(t, futureTime,
//       func(s *faketime.Session) error {
//           if err := s.Start(serviceCmd); err != nil { return err }
//           return s.Attach(postgresPID)
//       },
//       func(t *testing.T, s *faketime.Session) {
//           // all processes see futureTime here
//           s.SetTime(futureTime.Add(24 * time.Hour))
//           // assert expiry behaviour, etc.
//       },
//   )
func WithSession(t *testing.T, target time.Time,
    setup func(*Session) error,
    fn func(t *testing.T, s *Session))
```

All three helpers call `t.Helper()` and use `t.Cleanup` (not `defer`) so cleanup runs
even when `fn` calls `t.Fatal`.

---

### Progressive time advancement

`Session.SetTime` enables stepping through time within a single test:

```go
faketime.WithSession(t, base,
    func(s *faketime.Session) error {
        s.Start(serviceCmd)
        s.Attach(postgresPID)
        return s.Attach(redpandaPID)
    },
    func(t *testing.T, s *faketime.Session) {
        assertTokenValid(t, client)             // at base time

        s.SetTime(base.Add(23 * time.Hour))
        assertTokenValid(t, client)             // not expired yet

        s.SetTime(base.Add(25 * time.Hour))
        assertTokenExpired(t, client)           // should reject now
    },
)
```

Because `SetTime` completes in ~1 µs per process (issued concurrently), all processes
have the new time before the next RPC or Kafka message reaches them.

---

### Build constraints and platform notes

- All files in `pkg/faketime` carry `//go:build linux`.
- On non-Linux platforms (`GOOS=darwin`, `GOOS=windows`) the package does not compile.
  Tests that import it should be gated with the same build tag or use `t.Skip` behind a
  runtime check. An alternative stub package (`//go:build !linux`) that provides the same
  signatures but returns `errors.New("timeshift: not supported on this platform")` is
  worth adding so callers can compile cross-platform even if they can only run on Linux.
- The `FollowChild` path (`Start`, `WithProcess`, `Session.Start`) works inside Docker
  with `--cap-add SYS_PTRACE --security-opt seccomp=unconfined` — the same flags already
  required for `pkg/inject` tests.
- The `Attach` path (`Attach`, `WithPID`, `Session.Attach`) additionally requires
  `ptrace_scope ≤ 1` on the host. In most CI environments (`ubuntu-latest` on GitHub
  Actions) the default is 0, so this works without extra configuration.

---

### File layout

```
pkg/faketime/
├── faketime.go          # //go:build linux — Handle, Session, Start, Attach
├── faketime_stub.go     # //go:build !linux — same signatures, stub errors
├── testing.go            # //go:build linux — WithProcess, WithPID, WithSession
└── faketime_test.go     # //go:build linux — uses in-binary helper processes
```

---

### Tests

Follow the same in-binary helper pattern established in `pkg/inject`:

```go
const helperEnv = "EPOCHD_LOCALTIME_HELPER"

// TestLocaltimeHelper is the clock-printing child reused by all timeshift tests.
func TestLocaltimeHelper(t *testing.T) {
    if os.Getenv(helperEnv) != "1" { t.Skip() }
    for { fmt.Println(time.Now().Format(time.RFC3339Nano)); time.Sleep(100*time.Millisecond) }
}
```

**`TestStartSingleProcess`** — start the helper via `faketime.Start`, read timestamps
from its stdout, assert they are approximately `target`, then `Reset` and assert they
return to real time. This mirrors `TestInjectRoundTrip` in `pkg/inject`.

**`TestSessionMultipleProcesses`** — start two helper instances in one session, read
timestamps from both, assert both see `target`. Call `session.SetTime(target.Add(24h))`,
assert both update. Assert the inter-process update window (time between first and last
write) is under 10 ms.

**`TestSessionPartialFailure`** — add one valid PID and one invalid PID (e.g. 0) to a
session; assert `SetTime` returns an error that mentions the failing process but still
updates the valid one.

**`TestWithSession`** — end-to-end test of the `WithSession` helper: verify `t.Cleanup`
fires correctly by tracking whether Reset was called when `fn` returns normally and when
`fn` calls `t.Fatal`.

---

## Phase 26 — Conflict guard (prevent overlapping timeshifts)

**Goal**: reject `POST /timeshifts` when any container that would be targeted is already
tracked by an active timeshift. This prevents the silent corruption described in the
known-limitations section (two handles pointing at the same vDSO trampoline entry with one
stale state struct).

### Behaviour

`createTimeshift` checks all running containers matched by the selector against the current
registry before calling `agents.Inject`. If any container is already owned by a different
timeshift the request fails immediately — no injection is attempted, no partial state is
created.

**HTTP response**: `409 Conflict` with a JSON error body naming each conflicting container
and the timeshift ID that owns it.

**SDK helper**: `sdk.IsConflict(err) bool` mirrors `sdk.IsNotFound`.

### Implementation

**`cmd/controller/controller.go`**:

```go
// occupiedContainers returns a map of "namespace\x00pod\x00container" → timeshiftID
// for every container currently tracked by an active timeshift.
// Must be called with c.mu held for reading.
func (c *controller) occupiedContainers() map[string]string

// checkConflicts returns a *conflictError if any running container in pods is
// already tracked by another timeshift, nil otherwise.
func (c *controller) checkConflicts(ns string, pods []corev1.Pod) error
```

`createTimeshift` calls `checkConflicts` after listing pods, before any `Inject` calls.

**`cmd/controller/handlers.go`**: `isConflict(err) bool` helper; `handleCreateTimeshift`
returns `http.StatusConflict` when `isConflict` is true.

**`pkg/sdk/sdk.go`**: `IsConflict(err error) bool`.

### Tests

- `TestCreateTimeshiftConflictSamePod` — second timeshift with identical selector fails.
- `TestCreateTimeshiftConflictPartialOverlap` — broader selector overlapping one already-owned container fails.
- `TestCreateTimeshiftNoConflictAfterDelete` — after deleting first timeshift the same pod is targetable again.
- `TestHTTPCreateTimeshiftConflict` — HTTP layer returns 409 with descriptive message.

### Side-effects on existing tests

Three pre-existing tests (`TestListTimeshifts`, `TestHTTPListTimeshifts`,
`TestControllerRestoreGauge`) created two timeshifts for the same pod to exercise list /
restore logic. These were updated to use two distinct pods with separate selectors.

---

## Phase 27 — `faketimectl` subcommand completeness

**Goal**: make `faketimectl` a complete interface so operators never need to reach for curl.
Currently only `create` and `delete` are implemented; `update` and `status` are missing.

### New subcommands

**`faketimectl update <id> --time=<RFC3339>`**  
Calls `PATCH /timeshifts/<id>` and prints the updated timeshift. Accepts the same
`--time` flag format as `create`.

**`faketimectl status <id>`**  
Calls `GET /timeshifts/<id>/status` and prints a table of container statuses:

```
NAMESPACE  POD        CONTAINER  GENERATION  TARGET TIME           LAG
default    web-abc    app        4           2025-01-01T00:00:00Z  2ms
default    web-def    app        4           2025-01-01T00:00:00Z  3ms
```

### Implementation

- Add `updateCmd` and `statusCmd` to `cmd/faketimectl/main.go` (or split into subcommand
  files if the file is getting long).
- `updateCmd` reads `--time` flag, parses as RFC3339, calls `client.UpdateTimeshift`.
- `statusCmd` calls `client.TimeshiftStatus` and formats the `ContainerStatuses` slice
  as a tab-separated table using `text/tabwriter`.

### Tests

Unit tests mirroring the existing `create`/`delete` tests: fake HTTP server, assert the
right HTTP method + path is called, assert stdout formatting.

---

## Phase 28 — Structured logging (`log/slog`)

**Goal**: replace ad-hoc `fmt.Printf` / `log.Printf` calls in the controller and agent
with structured JSON logging via `log/slog` (stdlib, Go 1.21+). Structured logs are
consumable by Loki, Datadog, and Cloud Logging without custom parsing.

### Conventions

- One `*slog.Logger` per binary, constructed in `main` and threaded through via
  `context.WithValue` or explicit parameter.
- Log level controlled by `LOG_LEVEL` env var (`debug`, `info`, `warn`, `error`).
- All log lines include `component` (e.g. `"controller"`, `"agent"`) and `timeshift_id`
  where applicable.
- HTTP request logging: method, path, status, duration — one line per request.
- Injection events: `msg="inject"`, `pid`, `target_time`, `offset_sec`.

### Implementation

- `pkg/log/log.go`: thin helper that parses `LOG_LEVEL` and returns a `*slog.Logger`
  with `slog.NewJSONHandler(os.Stderr, opts)`.
- Replace all `fmt.Fprintf(os.Stderr, ...)` and `log.Printf` call sites.
- Add `slog.With("timeshift_id", id)` logger to the controller request context so all
  log lines within a single request share the ID.

### Tests

Log output is an observability concern, not a correctness concern — no dedicated log
tests. Existing unit tests should continue to pass; spot-check that no `fmt.Print*` calls
remain in hot paths via `go vet` or a grep assertion in CI.

---

## Phase 29 — TTL expiry events and counter

**Goal**: make timeshift expiry visible to pod owners without requiring them to watch
Prometheus. When the TTL sweep removes a timeshift, emit a Kubernetes `Event` on the
relevant pods and increment a `timeshift_expired_total` counter.

### Kubernetes Event

```
kubectl describe pod web-abc
Events:
  Type    Reason            Age   From               Message
  Normal  TimeshiftExpired  2m    epochd-controller  Timeshift abc123 (target: 2025-01-01T00:00:00Z) expired after 1h0m0s
```

The event is posted via `client-go`'s `EventRecorder` with:
- `Type`: `corev1.EventTypeNormal`
- `Reason`: `"TimeshiftExpired"`
- `Message`: human-readable string with timeshift ID, target time, and TTL duration.

### Prometheus

New counter `timeshift_expired_total` (no labels) incremented each time `sweepExpired`
removes a timeshift. Existing sweep metric (`timeshift_sweep_events_total`) tracks sweep
runs; this tracks individual expirations.

### Implementation

- Construct an `EventRecorder` in `main.go` using `record.NewEventRecorder`.
- Thread it into the controller as a field.
- In `sweepExpired`, after calling `Delete`, post an event on each pod that was targeted.
- Add the counter registration alongside existing Prometheus registrations.

### Tests

- `TestSweepEmitsEvent`: fake pod list + fake event recorder; assert event is posted with
  correct reason and message after TTL expires.
- `TestExpiredCounterIncrements`: assert `timeshift_expired_total` counter value after sweep.

---

## Phase 30 — Lease-based leader election

**Goal**: allow running two controller replicas without them stomping each other's
ConfigMap state. Only the elected leader processes write requests; standby replicas
return `503 Service Unavailable` immediately so clients can retry against the other
replica (or via a load balancer with retry).

### Mechanism

Use `k8s.io/client-go/tools/leaderelection` with a `coordination.k8s.io/Lease` object
named `epochd-leader` in the controller namespace.

- On startup, begin competing for the lease.
- Only start serving the HTTP API once the lease is held.
- On lease loss, stop accepting write requests and begin draining in-flight ones.
- On graceful shutdown, release the lease so the standby takes over immediately instead
  of waiting for the lease TTL.

### Configuration

New env vars (or flags):
- `LEADER_ELECTION=true/false` (default `false` — single-replica mode works as before)
- `LEASE_DURATION=15s`
- `RENEW_DEADLINE=10s`
- `RETRY_PERIOD=2s`

### Implementation

- Wrap the existing `http.ServeMux` in a middleware that checks an `atomic.Bool isLeader`
  flag and returns 503 for mutating requests when not the leader.
- Leader election goroutine runs `leaderelection.RunOrDie` in the background.

### Tests

Leader election cannot easily be tested without multiple processes. Add a unit test for
the middleware behaviour: when `isLeader=false`, `POST /timeshifts` returns 503; when
`isLeader=true`, it passes through.

---

## Phase 31 — Validating webhook admission controller

**Goal**: prevent new pods from being created (or restarted) into a namespace that has an
active timeshift targeting them. Without this, a pod restart during an active timeshift
causes it to start with the real clock until the agent re-injects it.

### Mechanism

A `ValidatingWebhookConfiguration` that intercepts `CREATE` and `UPDATE` (on restart)
operations for `Pods`. The webhook checks whether the pod's labels match any active
timeshift selector in its namespace, and if so — whether the pod is about to be created
fresh (not restarted from an existing injection). If a match is found and the controller
is not ready to immediately inject, the webhook rejects the admission.

In practice this is a guard against the race window, not a permanent block: the webhook
only rejects if the matching timeshift's agent is currently unreachable or the injection
would be delayed more than a configurable threshold.

### Configuration

- `WEBHOOK_PORT` (default `9443`) for TLS.
- TLS cert/key via mounted Secret or cert-manager `Certificate`.
- `WEBHOOK_REJECTION_THRESHOLD` (default `5s`) — reject only if agent RTT exceeds this.

### Implementation

- New `cmd/webhook/main.go` binary.
- `pkg/webhook/handler.go`: `AdmissionHandler` that calls the controller's `/resolve`
  endpoint to check selector overlap, then checks agent reachability.
- Kubernetes manifests: `deploy/webhook-deployment.yaml`,
  `deploy/webhook-service.yaml`, `deploy/webhook-config.yaml`.

### Tests

- Unit test the admission handler with a fake controller HTTP server.
- Integration test: kind cluster with the webhook deployed, assert that `kubectl create
  pod` is rejected when a matching timeshift is active.

---

## Phase 32 — `pkg/faketime` Attach path

**Goal**: complement Phase 25's `Start` (FollowChild) path with an `Attach` path that
injects into an already-running process by PID. This is useful when the process was
started by the test framework (testcontainers, `go test -exec`, an external binary) rather
than by the test itself.

### API addition to `pkg/faketime`

```go
// Attach injects fake time into an already-running process with the given pid.
// Requires CAP_SYS_PTRACE and ptrace_scope ≤ 1 on the host.
// Returns a Handle whose Close method resets the process to real time.
func Attach(pid int, target time.Time) (*Handle, error)

// WithPID is the testing helper equivalent of WithProcess for already-running PIDs.
func WithPID(t testing.TB, pid int, target time.Time, fn func(h *Handle))
```

### Permissions note

`Attach` wraps `inject.InjectAtTime` (which uses `PTRACE_ATTACH`). The caller needs:
- `CAP_SYS_PTRACE`, or
- the target process to be a child of the calling process, or
- `ptrace_scope = 0` on the host (`/proc/sys/kernel/yama/ptrace_scope`).

The docs and error messages should make this explicit.

### Tests

- `TestAttachSingleProcess`: spawn a helper, get its PID, call `Attach`, verify time
  offset same as `TestStartSingleProcess`.
- `TestWithPID`: use `WithPID` helper.
- Tests are `//go:build linux` and skip if `ptrace_scope > 1` (read from
  `/proc/sys/kernel/yama/ptrace_scope` with a helper).

---

## Phase 33 — Integration test harness

**Goal**: add a `make test-integration` target that is fully self-contained — it spins up
a kind cluster, builds and loads the controller and agent images, deploys epochd, runs the
e2e suite, and tears everything down. Currently the e2e suite assumes an existing cluster.

### Makefile targets

```makefile
test-integration: kind-up deploy-epochd e2e kind-down

kind-up:
	kind create cluster --name epochd-test --config deploy/kind-config.yaml

kind-down:
	kind delete cluster --name epochd-test

deploy-epochd: docker-build
	kind load docker-image epochd-controller:dev --name epochd-test
	kind load docker-image epochd-agent:dev --name epochd-test
	kubectl apply -f deploy/
	kubectl -n epochd rollout status deployment/epochd-controller --timeout=60s
	kubectl -n epochd rollout status daemonset/epochd-agent --timeout=60s

e2e:
	go test ./e2e/... -v -timeout 5m
```

### `deploy/kind-config.yaml`

A minimal kind config that:
- Uses a single control-plane node.
- Mounts `/proc` so the agent's vDSO patching works inside containers.
- Sets `ptrace_scope = 0` via a `extraMounts` / initContainer workaround.

### CI integration

Add a new GitHub Actions workflow `e2e.yml` that runs `make test-integration` on
`push` to `main` and on PRs that touch `cmd/`, `pkg/`, or `deploy/`. Use a
`ubuntu-latest` runner (kind works on GitHub Actions without Docker-in-Docker).

---

## Phase 38 — `pkg/faketime` API ergonomics

### Goal

Fill four small gaps in the `pkg/faketime` public API that require callers to
track state themselves or produce confusing errors.

### `Handle.EffectiveTime() time.Time`

`effectiveTime()` already exists as an unexported method. Export it:

```go
// EffectiveTime returns the fake time the process currently sees.
// For advancing handles this is time.Now() + offset; for frozen handles
// it is the pinned instant.
func (h *Handle) EffectiveTime() time.Time {
    h.mu.Lock()
    defer h.mu.Unlock()
    return h.effectiveTime()
}
```

Useful for test assertions without having to track the target time separately.

### `Handle.PID() int`

```go
// PID returns the PID of the target process.
func (h *Handle) PID() int { return h.h.PID }
```

Callers that spawn via `faketime.Start` currently must retain `cmd.Process.Pid`
themselves. This exposes it directly from the handle.

### `Session.Close() error`

Currently callers must track `[]*exec.Cmd` alongside the session to kill child
processes outside of the test helpers. `Session` already stores `cmds` internally:

```go
// Close resets all handles to the real clock, kills all child processes
// started via Session.Start, and waits for them to exit.
// Errors from Reset and Wait are joined and returned.
func (s *Session) Close() error {
    resetErr := s.Reset()
    s.mu.Lock()
    cmds := make([]*exec.Cmd, len(s.cmds))
    copy(cmds, s.cmds)
    s.mu.Unlock()
    var errs []error
    for _, cmd := range cmds {
        cmd.Process.Kill() //nolint:errcheck
        if err := cmd.Wait(); err != nil {
            errs = append(errs, err)
        }
    }
    return errors.Join(append(errs, resetErr)...)
}
```

### `Handle.IsAlive() bool`

A dead process produces an opaque `process_vm_writev: no such process` error on
the next `SetTime` call. `IsAlive` surfaces this proactively:

```go
// IsAlive reports whether the target process is still running.
func (h *Handle) IsAlive() bool {
    err := syscall.Kill(h.h.PID, 0)
    return err == nil || errors.Is(err, syscall.EPERM)
}
```

`EPERM` means the process exists but is owned by another user — still alive.
`ESRCH` means it is gone.

### Files changed

- `pkg/faketime/faketime.go` — add `EffectiveTime`, `PID`, `IsAlive` to `Handle`; add `Close` to `Session`
- `pkg/faketime/faketime_stub.go` — add stub implementations returning `errNotSupported`
- `pkg/faketime/example_test.go` — add `ExampleHandle_EffectiveTime`, `ExampleHandle_IsAlive`, `ExampleSession_Close`
