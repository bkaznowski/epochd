# Deploying epochd

```bash
kubectl apply -f deploy/rbac.yaml            # namespace, service account, RBAC

# Credentials come first: the agent and the controller refuse to start without them.
kubectl -n epochd create secret generic epochd-auth \
  --from-literal=api-token="$(openssl rand -hex 32)" \
  --from-literal=agent-token="$(openssl rand -hex 32)"

kubectl apply -f deploy/daemonset.yaml       # one agent per node (CAP_SYS_PTRACE only, not privileged)
kubectl apply -f deploy/controller-deployment.yaml
```

`make deploy` does the same for a local cluster (and `make secret` creates just the
Secret, leaving an existing one alone).

## Why there are two tokens

| Secret key | Who presents it | Who requires it |
|------------|-----------------|-----------------|
| `api-token` | clients of the controller API (`faketimectl`, the Go SDK, your tests) | the controller |
| `agent-token` | the controller, when it calls an agent | each node agent |

The agent can change the clock of any process on its node (it runs with `hostPID` and attaches to them), so it must not trust anyone who merely reaches its port. It is not a privileged container, but see "What the agent can do" below before treating that as safe.
It is only given
`agent-token`: a client that holds `api-token` cannot call the agents directly.

Clients pass their token as `Authorization: Bearer <token>`:

```bash
export EPOCHD_URL=http://epochd-controller.epochd.svc
export EPOCHD_TOKEN="$(kubectl -n epochd get secret epochd-auth -o jsonpath='{.data.api-token}' | base64 -d)"
faketimectl list
```

```go
client := sdk.NewClient(url).WithToken(token)
```

`/healthz` and `/metrics` are served without a token so probes and Prometheus keep working.

## TLS

A bearer token sent over a plaintext connection can be read off the wire. Both
directions support TLS, and mutual TLS:

| Component | Flags |
|-----------|-------|
| agent (server) | `--tls-cert-file`, `--tls-key-file`, and `--tls-client-ca-file` to require client certificates |
| controller (to the agents) | `--agent-ca-file`, `--agent-client-cert-file` + `--agent-client-key-file`, `--agent-server-name` |

The controller dials agents by node IP, so either issue agent certificates with IP SANs
or set `--agent-server-name` to the name they carry. With `--tls-client-ca-file` set, a
verified client certificate counts as authentication and the token becomes optional.

Network policies cannot protect the agent: it runs with `hostNetwork`, which they do
not govern. The token (and TLS) is the access control.

## Local development

Both binaries accept `--allow-unauthenticated`, which turns authentication off and logs a
warning. Never use it where the port is reachable by anything you do not control.

## Ports

The agent listens on **9433**, not 9100: 9100 is Prometheus node-exporter's default, and
under `hostNetwork` the two would fight over it. The controller's `--agent-port` and the
DaemonSet's `containerPort`/`hostPort` must agree; `go test ./deploy` checks that.

## What the agent can do

The agent is **not** a privileged container. It runs as root with every capability
dropped except `CAP_SYS_PTRACE`, the runtime-default seccomp profile, a read-only root
filesystem and no privilege escalation. Capabilities beyond `CAP_SYS_PTRACE` turned out
not to be needed: the full end-to-end suite (inject, update, delete, TTL expiry, agent
crash and restart) passes on a kind cluster with exactly that.

That is a smaller blast radius than `privileged: true` (no devices, no mounts, no other
capabilities), but not a small one. `CAP_SYS_PTRACE` together with `hostPID` lets the agent
attach to and write into **any process on the node**, including root-owned ones, which is
close to root on the node. The authentication on the agent port is what stands between that
and everything that can reach it; treat the tokens accordingly and do not run epochd in a
cluster you would not already give root on its nodes to the people who can use it.

Two environments need a different security context:

- **AppArmor** (Ubuntu nodes, for example): the container runtime's default profile
  confines ptrace to processes under the same profile, which stops the agent tracing other
  pods. Run the agent unconfined: `appArmorProfile: {type: Unconfined}` in the container's
  `securityContext` (Kubernetes 1.30+; earlier versions use the
  `container.apparmor.security.beta.kubernetes.io/agent: unconfined` annotation).
  This is not in the shipped manifest because a cluster older than 1.30 rejects the field.
- **SELinux** (OpenShift, RHEL): the agent needs a type that may ptrace other containers,
  such as `spc_t`, via `seLinuxOptions`.

If neither fixes it, `privileged: true` is the fallback. The kind test cluster has neither
AppArmor nor SELinux, so these two have not been tried.

The Yama `kernel.yama.ptrace_scope` setting is bypassed by `CAP_SYS_PTRACE` at 0 to 2, but
a node set to 3 forbids ptrace for everyone and the agent cannot work there.
