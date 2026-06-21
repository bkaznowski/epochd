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
- [x] **Phase 25 — Local process injection** (`pkg/localtime`: `Session`, `Start`/`Attach`, `WithSession` helper; non-Kubernetes, Linux only)
- [x] **Phase 26 — Conflict guard** (reject `POST /timeshifts` with `409 Conflict` when any matched container is already in an active timeshift; `sdk.IsConflict`)
- [ ] **Phase 27 — `faketimectl` subcommand completeness** (`update <id>` and `status <id>` subcommands; tabwriter output)
- [ ] **Phase 28 — Structured logging** (`log/slog` JSON handler; `LOG_LEVEL` env var; per-request logger with `timeshift_id`)
- [ ] **Phase 29 — TTL expiry events and counter** (Kubernetes `Event` on pod when timeshift expires; `timeshift_expired_total` Prometheus counter)
- [ ] **Phase 30 — Lease-based leader election** (`coordination.k8s.io/Lease`; standby replicas return `503`; `LEADER_ELECTION` env var)
- [ ] **Phase 31 — Validating webhook admission controller** (reject pod creation when matching timeshift is active and agent is unreachable; separate `cmd/webhook` binary)
- [ ] **Phase 32 — `pkg/localtime` Attach path** (`Attach(pid, target)` + `WithPID` helper; requires `CAP_SYS_PTRACE`; skip test if `ptrace_scope > 1`)
- [ ] **Phase 33 — Integration test harness** (`make test-integration`; kind cluster lifecycle; `e2e.yml` GitHub Actions workflow)

---

## After going public

- [ ] Update the `your-org` badge URLs in `README.md` once the repo exists and CI has run
  at least once (the badge won't render until the workflow file is pushed).
- [ ] Consider adding a `CONTRIBUTING.md` if you want to guide external contributors.
