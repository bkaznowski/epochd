# TODO

Things that require your input or manual action before the project is fully ready.

---

## One-time setup

- [x] **Choose your GitHub org/username** and replace every occurrence of `your-org` in the
  following files:
  - `README.md` — two badge URLs (CI link and badge image)
  - `deploy/daemonset.yaml` — `image: ghcr.io/your-org/epochd-agent:latest`
  - `deploy/controller-deployment.yaml` — `image: ghcr.io/your-org/epochd-controller:latest`
  - `CONTEXT.md` — module path example (`github.com/your-org/epochd/sdk`)

- [x] **Update the LICENSE copyright line** — replace `"epochd contributors"` with your
  name or organisation name in `LICENSE`.

- [x] **Create the GitHub repository** — `https://github.com/bkaznowski/epochd`. Make it
  public when you're ready to open-source.

- [x] **Initialise git and push**:
  ```bash
  git init
  git add .
  git commit -m "initial commit"
  git remote add origin https://github.com/<your-org>/epochd.git
  git push -u origin main
  ```

- [ ] **Enable branch protection on `main`** (GitHub → Settings → Branches):
  - Require the CI workflow to pass before merging
  - Require at least one approving review if you plan to accept external contributions

---

## Remaining phases

- [x] **Phase 14 — GitHub Actions CI** (`.github/workflows/ci.yml`, `.golangci.yml`)

- [x] **Phase 15 — Local cluster e2e** (`Makefile`, `e2e/e2e_test.go`)
- [x] **Phase 16 — List timeshifts** (`GET /timeshifts`, `ListTimeshifts` in SDK)
- [x] **Phase 17 — Health endpoint + `WithTimeT`** (`/healthz`, ergonomic test helper)
- [x] **Phase 18 — Handle recovery** (re-inject on agent restart; pod watcher for container restarts)
- [x] **Phase 19 — Prometheus metrics** (active timeshifts, inject/settime counters, sweep events)
- [x] **Phase 20 — Controller restart recovery** (persist registry to ConfigMap; reload on startup)
- [x] **Phase 21 — Graceful agent shutdown** (reset all handles on SIGTERM before exit)
- [x] **Phase 22 — Dry-run / resolve mode** (`GET /resolve?namespace=…&selector=…`)
- [x] **Phase 23 — Agent handle status RPC** (`GetStatus` → live generation + targetTime from trampoline)
- [x] **Phase 25 — Local process injection** (`pkg/faketime`: `Session`, `Start`/`Attach`, `WithSession` helper; non-Kubernetes, Linux only)
- [x] **Phase 26 — Conflict guard** (reject `POST /timeshifts` with `409 Conflict` when any matched container is already in an active timeshift; `sdk.IsConflict`)
- [x] **Phase 27 — `faketimectl` subcommand completeness** (`update <id>` and `status <id>` subcommands; tabwriter output)
- [x] **Phase 28 — Structured logging** (`log/slog` JSON handler; `LOG_LEVEL` env var; per-request logger with `timeshift_id`)
- [x] **Phase 29 — TTL expiry events and counter** (Kubernetes `Event` on pod when timeshift expires; `timeshift_expired_total` Prometheus counter)
- [ ] **Phase 30 — Lease-based leader election** (`coordination.k8s.io/Lease`; standby replicas return `503`; `LEADER_ELECTION` env var)
- [ ] **Phase 31 — Validating webhook admission controller** (reject pod creation when matching timeshift is active and agent is unreachable; separate `cmd/webhook` binary)
- [x] **Phase 32 — `pkg/faketime` Attach path** (`Attach(pid, target)` + `WithPID` helper; requires `CAP_SYS_PTRACE`; skip test if `ptrace_scope > 1`)
- [x] **Phase 33 — Integration test harness** (`make test-integration`; kind cluster lifecycle; `e2e.yml` GitHub Actions workflow)
- [x] **Phase 38 — `pkg/faketime` API ergonomics** — `Handle.EffectiveTime()`, `Handle.PID()`, `Handle.IsAlive()`, `Session.Close()`

---

## After going public

- [ ] Update the `your-org` badge URLs in `README.md` once the repo exists and CI has run
  at least once (the badge won't render until the workflow file is pushed).
- [ ] Consider adding a `CONTRIBUTING.md` if you want to guide external contributors.

---

## v0.8 hardening (done)

- [x] Trampoline rebuilt from C instead of assembly; injection safe on running, multi-threaded
  processes (idempotent, `Adopt`, `Uninstall`, `ErrHooksLost`)
- [x] Agent covers every process in a container, has a `Release` RPC, moved off port 9100,
  and runs with `CAP_SYS_PTRACE` only
- [x] Authentication on the agent and the controller (bearer token, TLS, mTLS); refuse to
  start without it unless `--allow-unauthenticated`
- [x] Controller correctness: snapshots, per-timeshift locking, delete and TTL retry until
  containers are really back on the real clock, take-over after an agent crash
- [x] Preload: musl and glibc from one library, attach counter and CLI warning, seqlock
  fallback, absolute-deadline translation, `Session.Prune` keeps clocks still in use
- [x] `WithAutoMode`, per-process modes, tracker sweep for children of late threads
- [x] E2E on a real cluster for delete, TTL expiry, agent SIGKILL and graceful restart
- [x] CI: actions pinned to SHAs, `-race`, pinned golangci-lint, Dependabot; `release.yml`
  with a CI gate, atomic push, resumability and a `dry_run` input

## Open

- [ ] **Phase 30 — leader election** and **Phase 31 — validating webhook** (see above)
- [ ] **arm64** (trampoline payload, vDSO patch, images) — `FUTURE.md`
- [ ] **TLS by default**: a cert-manager example, or certificates generated by `make deploy`;
  today a bearer token travels in the clear unless TLS is configured
- [ ] **AppArmor / SELinux**: try the agent on such nodes and record the security context it
  needs (`deploy/README.md` says what is expected but has not been tried)
- [ ] **`PTRACE_O_TRACECLONE`** to remove the up-to-100 ms window for a stray child that execs
- [ ] Preload: `mq_timedreceive`/`mq_timedsend` and `dup()`ed timerfds are not translated;
  vDSO mode translates no absolute deadlines
- [ ] A session-level warning (not only the CLI's) when a preload clock was never loaded
- [ ] Enable branch protection on `main` (listed under one-time setup above)
