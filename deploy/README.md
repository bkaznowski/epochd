# Deploying epochd

```bash
kubectl apply -f deploy/rbac.yaml            # namespace, service account, RBAC

# Credentials come first: the agent and the controller refuse to start without them.
kubectl -n epochd create secret generic epochd-auth \
  --from-literal=api-token="$(openssl rand -hex 32)" \
  --from-literal=agent-token="$(openssl rand -hex 32)"

kubectl apply -f deploy/daemonset.yaml       # one privileged agent per node
kubectl apply -f deploy/controller-deployment.yaml
```

`make deploy` does the same for a local cluster (and `make secret` creates just the
Secret, leaving an existing one alone).

## Why there are two tokens

| Secret key | Who presents it | Who requires it |
|------------|-----------------|-----------------|
| `api-token` | clients of the controller API (`faketimectl`, the Go SDK, your tests) | the controller |
| `agent-token` | the controller, when it calls an agent | each node agent |

The agent is privileged (it runs with `hostPID` and can attach to any process on the
node), so it must not trust anyone who merely reaches its port. It is only given
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
