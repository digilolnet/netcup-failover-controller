# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

A Kubernetes controller-runtime operator that routes netcup failover IPs to healthy cluster nodes via the netcup SCP REST API, using github.com/digilolnet/go-netcup-scp. Uses a `NetcupFailoverIP` CRD (`netcup.digilol.net/v1alpha1`). (The netcup SOAP webservice this originally targeted was shut down on 2026-05-01.)

## Commands

```bash
go build ./...
go test ./...
go test -run TestName ./internal/netcup/   # run a single test
go vet ./...
golangci-lint run
```

## Layout

```
api/v1alpha1/          # CRD types and scheme registration
internal/controller/   # reconciler (failoverip_controller.go) + node selection (nodes.go)
internal/netcup/       # SCP REST API session (client.go) + routing/task logic (routing.go); wraps go-netcup-scp
config/crd/            # CRD manifest
config/rbac/           # ClusterRole, ClusterRoleBinding, ServiceAccount
config/manager/        # Deployment manifest
config/samples/        # example NetcupFailoverIP resources
```

## Key design decisions

- IPs in the CRD spec use CIDR notation (`198.51.100.1/32`, `2001:db8::/64`); the controller matches them against the account's failover IPs (`ListFailoverIPv4`/`v6`) to find the numeric failover ID the REST API needs
- Credentials are per-resource via `credentialsSecret` (a `LocalObjectReference` — name only) whose `token` key holds the OAuth2 token JSON; the numeric SCP user ID is extracted from the access token's `id` JWT claim
- Credentials Secrets always live in the controller's own namespace (`CREDENTIALS_NAMESPACE`, a downward-API env var), matched by a namespaced Role on Secrets — cluster-wide secret access and confused-deputy reads/writes are impossible by construction
- `NETCUP_SCP_API_URL` / `NETCUP_SCP_AUTH_URL` env vars (Helm: `netcup.apiUrl`/`netcup.authUrl`) override the SCP endpoints via `netcup.Config`; empty = production
- Metrics server is disabled (`Metrics.BindAddress: "0"`); the chart ships a NetworkPolicy (egress DNS + TCP 443/6443 only) and a ValidatingAdmissionPolicy (K8s 1.30+) denying kubelets changes to their own node's server-name annotation — both toggleable in values.yaml
- CI runs vet, race tests, golangci-lint, govulncheck, and gosec (`#nosec` suppressions carry justifications); actions are SHA-pinned; Dependabot covers gomod + actions; release images are cosign-signed (keyless) with provenance and SBOM
- The binary doubles as a bootstrap CLI: `netcup-failover-controller login [--secret ns/name]` (also via `kubectl exec … /manager login`) runs the OAuth2 device flow — print URL, user logs in in the browser, poll for the token — and creates/updates the credentials Secret; login code lives in `login.go` (package main), the Secret key constant is `netcup.TokenSecretKey`
- Refreshed OAuth tokens are written back to the credentials Secret (RBAC grants `update` on secrets) so the stored refresh token never expires from disuse
- `NetcupFailoverIP` is **cluster-scoped**
- `corev1.Secret` is excluded from the controller-runtime cache — credential reads always go directly to the API server
- Node selection is deterministic (`hash(resource.name) % eligible nodes`), preferring nodes not already hosting another failover IP group; the group stays on its current node while it remains healthy and eligible
- Controller watches `Node` objects with a predicate (readiness flips, label or annotation changes — not heartbeats); matching events re-enqueue all `NetcupFailoverIP` resources
- Reconcile no-ops when the `Routed` condition is True at the current generation and the current node is still eligible — this check runs before any Secret read or API call, except when the drift check is due
- Per-IP progress is not tracked in status: the API's failover IP listings authoritatively report where each IP is routed, and IPs already on the target server are skipped — a rate limit or crash mid-group never re-routes IPs that already succeeded
- Routing calls are async (HTTP 202 + task); the controller polls the task until FINISHED, and ERROR/CANCELED is a routing failure. HTTP 429 (10 route requests per 5 minutes) requeues after 5 minutes; other failures set the `Routed` condition and return an error for backoff requeue — there is no inner retry loop
- Each node requires annotation `netcup.digilol.net/server-name` naming its SCP server; the controller lists the account's servers once per routing reconcile (`Session.ServerIDsByName`) and only places a group on nodes whose annotation resolves in *its* account — multi-account clusters work without per-account node labels (unusable nodes → `NoAccountNodes` condition)
- Two replicas with leader election; must run on control-plane nodes, spread by pod anti-affinity. The lease lives in the controller's own namespace (same `CREDENTIALS_NAMESPACE` value, namespaced Role on leases/events) and is released on shutdown (`LeaderElectionReleaseOnCancel`)
- Spec errors that retrying cannot fix (`InvalidIP`, `InvalidNodeSelector`) set the condition and return nil — no backoff requeue; the spec fix bumps the generation
- golangci-lint enables `revive`: every exported identifier needs a doc comment
- Only spec changes (`GenerationChangedPredicate`) and Node events trigger a reconcile — the controller's own status writes must not, or they bypass error backoff and burn the routing rate limit
- Every SCP API call is bounded by `Session.CallTimeout` (30s default); the library's HTTP client and the reconcile context have no deadline of their own. `Session` zero values mean the defaults
- The library's background token auto-refresh is off (`auth.WithAutoRefresh(false)`): sessions live for one reconcile and refresh on demand, so `onRefresh` runs synchronously on the reconcile goroutine. `persistToken` is detached from cancellation (`context.WithoutCancel` + timeout) and retries on conflict so a rotated refresh token is never dropped
- Condition messages are truncated to 1024 bytes (upstream errors can embed whole response bodies)
- The release workflow calls CI (`workflow_call`) and publishes only if it passes; the release tag is injected as `main.version` via ldflags and logged at startup (`/manager version`)
- The Dockerfile base image is pinned by digest; Dependabot (docker ecosystem) bumps it, and k8s modules / actions are grouped into single PRs
- Drift check: a routed group is re-verified against netcup every `ResyncInterval` (`RESYNC_INTERVAL` env / Helm `resyncInterval`, default 5m, `0` disables) via the read-only `Session.Misrouted`; drift flips the condition to `Routing` and re-routes. Last-verified times are in-memory (a new leader verifies everything once), an uneventful check writes no status, and a check that cannot reach netcup returns an error without touching the condition
- `Routed=True` means netcup's routing, not reachability: the controller neither configures nor probes addresses on nodes (README covers DAD/loopback and using `nodeSelector` labels to take a node out of rotation)
- `internal/netcup` keeps a narrow `API` interface over `*scp.Client` so tests inject a mock, and owns all netcup-domain logic (`ParseRoutes`, `Session.ServerIDByName`, `Session.EnsureRouted`, task polling) — the controller only does Kubernetes-shaped work; `go-netcup-scp`'s generated types live in its `internal/`, so test fixtures populate them via `json.Unmarshal`
- Condition reasons are constants in `api/v1alpha1` (`ReasonNodeSelected` etc.)
- The CRD manifest is handwritten (no controller-gen); it exists in both `config/crd/` and `charts/.../templates/crds/` — keep the two copies in sync
- `agent/` (vendored Go skills with example `.go` files) is fenced off from the module by its own `go.mod`
