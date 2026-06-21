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
- [ ] **Phase 19 — Prometheus metrics** (active timeshifts, inject/settime counters, sweep events)

---

## After going public

- [ ] Update the `your-org` badge URLs in `README.md` once the repo exists and CI has run
  at least once (the badge won't render until the workflow file is pushed).
- [ ] Consider adding a `CONTRIBUTING.md` if you want to guide external contributors.
