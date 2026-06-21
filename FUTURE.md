# Future improvements

Larger or lower-priority ideas that are worth doing eventually but don't fit
into the current implementation roadmap. Each item below notes what it unlocks
and what the main complexity is.

---

## Security — authentication on the controller API

**What it unlocks**: safe exposure of the controller inside a shared cluster.
Right now any pod that can reach the controller service can shift the clock of
any other pod in the cluster.

**Options (in increasing complexity)**:

1. **Shared token** — a static token passed in an `Authorization: Bearer`
   header; configured via a Kubernetes `Secret` mounted into the controller and
   provided by test clients via an environment variable. Simple, zero extra
   dependencies.

2. **Kubernetes `TokenReview`** — the client presents a ServiceAccount token;
   the controller validates it against the API server with a `TokenReview`
   request. Ties access control to standard Kubernetes RBAC.

3. **mTLS** — mutual TLS between the test client and the controller, and
   between the controller and each node agent. Most operationally complex;
   warranted if epochd is deployed in a multi-tenant or production-adjacent
   cluster.

**Recommended starting point**: option 1 (shared token) gives immediate
protection with minimal code. Gate it behind a `--auth-token` flag; if the
flag is empty, auth is disabled (useful for local development).

---

## Multi-architecture support (linux/arm64)

**What it unlocks**: running epochd on ARM-based nodes — AWS Graviton,
Apple Silicon via Docker Desktop Linux VM, Raspberry Pi clusters.

**What's needed**:

- A new assembly payload for AArch64. The trampoline logic is the same; the
  instruction encoding is entirely different. Key difference: AArch64's `B`
  (unconditional branch) has only a ±128 MB range, which is often enough but
  not guaranteed. The safe approach is an indirect branch:
  ```asm
  ; patch site (4 bytes at clock_gettime entry):
  ldr  x16, #8        ; load absolute address from the literal pool (8 bytes ahead)
  br   x16            ; indirect branch — unlimited range
  ; literal pool (8 bytes):
  .quad <trampoline_page_address>
  ```
  This requires patching 12 bytes instead of 5, and needs 12 contiguous
  writable bytes at the vDSO entry — confirm this is available by inspecting
  the AArch64 vDSO's `clock_gettime` prologue.

- A build-time arch dispatch in `pkg/trampoline` — embed either
  `trampoline_amd64.bin` or `trampoline_arm64.bin` based on `GOARCH`, and
  expose the correct `StateOffset` constant for each.

- A separate `pkg/inject` code path for the JMP patch (5-byte x86-64 vs
  12-byte AArch64 sequence).

- Multi-arch Docker builds in the CI workflow: add `linux/arm64` to the
  `platforms` field in `build-push-action`.

---

## Helm chart

**What it unlocks**: one-command install with configurable values; versioned
releases that users can pin with `helm upgrade`.

**What's needed**:

- A `charts/epochd/` directory with `Chart.yaml`, `values.yaml`, and templates
  for the Namespace, ServiceAccount, ClusterRole, ClusterRoleBinding,
  DaemonSet, Deployment, and Service — essentially templated versions of the
  existing manifests in `deploy/`.

- Key `values.yaml` fields:
  ```yaml
  image:
    agent:      { repository: ghcr.io/bkaznowski/epochd-agent,      tag: latest }
    controller: { repository: ghcr.io/bkaznowski/epochd-controller,  tag: latest }
  agent:
    port: 9100
    resources: { requests: { cpu: 10m, memory: 32Mi }, limits: { cpu: 200m, memory: 128Mi } }
  controller:
    port: 8080
    sweepInterval: 30s
    replicas: 1
    resources: { requests: { cpu: 10m, memory: 32Mi }, limits: { cpu: 200m, memory: 128Mi } }
  ```

- A `helm lint` step in CI.

- Published to a GitHub Pages chart repository or the GitHub OCI registry
  (`oci://ghcr.io/bkaznowski/charts/epochd`).

---

## Controller high availability (leader election)

**What it unlocks**: running multiple controller replicas so a pod eviction
doesn't cause a gap in TTL sweeping or timeshift management.

**The problem**: the timeshift registry is in-memory and per-process. Running
two replicas means split state — `POST /timeshifts` on replica A creates a
timeshift that replica B knows nothing about, so `GET /timeshifts/{id}` on
replica B returns 404.

**Options**:

1. **Leader election (simplest)** — use `k8s.io/client-go/tools/leaderelection`
   to elect a single active replica via a Kubernetes `Lease` object. Only the
   leader serves write requests; followers return HTTP 503 or redirect. Replica
   count can be >1 for fast failover but only one is active at a time.

2. **Persistent state** — store the timeshift registry in a Kubernetes
   `ConfigMap` or `Secret`, updated on every write. Any replica can serve any
   request. More complex: you need optimistic concurrency (resource version
   checks) to avoid write conflicts.

3. **External store** — use Redis or etcd as the authoritative state store.
   Operationally heavier; probably not worth it for a testing tool.

**Recommended starting point**: option 1. It requires ~50 lines of
`leaderelection` wiring and gives you HA with minimal complexity.

---

## `CLOCK_MONOTONIC` interception

**What it unlocks**: shifting time for applications that measure elapsed time
with `CLOCK_MONOTONIC` — timeouts, rate limiters, cache TTLs, and anything
that uses `time.Since` in Go (which uses `CLOCK_MONOTONIC` internally).

**The problem**: the current trampoline only intercepts `CLOCK_REALTIME`
(clk_id == 0). `CLOCK_MONOTONIC` (clk_id == 1) passes through unchanged, so
its value is inconsistent with `CLOCK_REALTIME` after injection.

**What's needed**:

- Extend the trampoline to handle `clk_id == 1` with the same offset — add a
  second `cmp edi, 1 / je .apply_offset` branch before `.done`. The offset
  applied to `CLOCK_MONOTONIC` should be the same seconds/nanoseconds value so
  both clocks are shifted consistently.

- Extend the `enabledMask` field in the state struct to use bit 1 for
  `CLOCK_MONOTONIC` (currently unused), so callers can choose to shift only
  `CLOCK_REALTIME`, only `CLOCK_MONOTONIC`, or both.

- Update `TestStateOffsetRegression` and any trampoline layout tests if the
  binary size changes after editing the assembly.

**Caveat**: shifting `CLOCK_MONOTONIC` is semantically surprising — the whole
point of the monotonic clock is that it only moves forward and never jumps.
Applications that use it for measuring elapsed time will see their intervals
compress or expand. This is probably what you want in a time-shift test (a
1-hour timeout should appear to fire immediately if you've shifted time forward
1 hour), but it may confuse applications that compare `CLOCK_MONOTONIC` and
`CLOCK_REALTIME` and expect them to diverge only by a stable skew.
