# netcup-failover-controller

A Kubernetes controller that automatically routes [netcup](https://www.netcup.de) failover IPs to healthy cluster nodes via the netcup SCP REST API (using [go-netcup-scp](https://github.com/digilolnet/go-netcup-scp)).

## Overview

The controller watches `NetcupFailoverIP` custom resources. Each resource defines a group of failover IPs (e.g. one IPv4 + one IPv6) that are always routed to the same node. When a node becomes unhealthy, the controller re-routes to a different healthy node automatically.

Multiple `NetcupFailoverIP` resources are spread across different nodes for bandwidth splitting — the controller avoids placing two groups on the same node when alternatives exist.

## How It Works

1. A `NetcupFailoverIP` resource is created with a list of IPs and a reference to a Secret containing a netcup SCP OAuth token.
2. The controller lists healthy nodes (optionally filtered by a label selector).
3. It picks a node deterministically (`hash(resource name) % eligible nodes`), preferring nodes not already hosting another failover IP group.
4. It resolves the node's SCP server (via the `netcup.digilol.net/server-name` annotation) and routes each failover IP to it through the SCP REST API, skipping IPs the API already reports as routed there.
5. Status is updated with the current node and a `Routed` condition.
6. Node changes trigger re-evaluation for all resources; a group stays on its current node while that node remains healthy and eligible.

netcup rate-limits failover routing (10 requests per 5 minutes). When the limit is hit the controller records a `RateLimited` condition and retries after 5 minutes — IPs that already routed are never re-routed, so a large group converges across windows.

The controller runs with two replicas and leader election — one active, one standby.

## Prerequisites

- Each node must have the following annotation set:

| Annotation                       | Example value          |
| -------------------------------- | ---------------------- |
| `netcup.digilol.net/server-name` | `v1234567890123456789` |

## Node Network Configuration

The controller routes traffic to a node at the netcup network level, but the node's OS must also have the failover IPs configured on its network interface — otherwise the kernel will drop incoming packets.

Configure the failover IPs on **all eligible nodes** (not just the currently active one), so that when a failover switches routing to a different node, that node is immediately ready to accept packets.

### IPv4

Add the failover IP as an additional address on the node's interface:

```yaml
# patch.yaml
machine:
  network:
    interfaces:
      - interface: <interface-name> # e.g. eth0, ens3
        addresses:
          - 198.51.100.1/32
```

### IPv6 /64

Two addresses are required on the interface:

1. **The failover /64 prefix** — so the kernel accepts packets destined anywhere in the range.
2. **The node's EUI-64 host address** — netcup's router uses this as the next-hop when forwarding packets for the /64. Without it the router's NDP goes unanswered and no traffic arrives.

The EUI-64 host address is derived from the interface MAC address. If your cluster provisioning tool configures the native IPv6 as `2001:db8:1:2::/64` (the `::` network address), you need to also add the EUI-64 host address explicitly.

Derive it from the link-local address: if the link-local is `fe80::a8bb:ccff:fedd:eeff`, the global EUI-64 address in the native /64 is `2001:db8:1:2:a8bb:ccff:fedd:eeff`.

```yaml
# patch.yaml
machine:
  network:
    interfaces:
      - interface: <interface-name>
        addresses:
          - 2001:db8::/64 # the failover /64
          - 2001:db8:1:2:a8bb:ccff:fedd:eeff/64 # native EUI-64 host address
```

### Applying on Talos Linux

```bash
talosctl patch mc --nodes <node-ip> --patch @patch.yaml
```

## Installation

### Helm (recommended)

```bash
helm repo add netcup https://digilolnet.github.io/netcup-failover-controller
helm install netcup-failover-controller netcup/netcup-failover-controller
```

To pin to a specific version:

```bash
helm install netcup-failover-controller netcup/netcup-failover-controller --version <version>
```

### Manual

```bash
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
kubectl apply -f config/manager/
kubectl apply -f config/admission/   # optional: node-annotation protection (Kubernetes 1.30+)
```


## Usage

### 1. Log in (creates the credentials Secret)

The controller authenticates against the SCP REST API with an OAuth2 token obtained via the device flow. The `login` subcommand walks you through it and stores the token in the cluster: it prints a URL, you open it in your browser and log in, and the tool picks the token up automatically.

Run it in-cluster through one of the controller pods:

```bash
kubectl exec -it -n netcup-system deploy/netcup-failover-controller -- /manager login
```

Or locally with your kubeconfig, if you have the binary:

```bash
netcup-failover-controller login --secret netcup-system/netcup-credentials
```

Either way it creates (or updates) the referenced Secret with the token under the `token` key. The controller then writes refreshed tokens back into the Secret, so the one-time login stays valid indefinitely.

Alternatively, seed the Secret from a token file written by the [`netcup-scp` CLI](https://github.com/digilolnet/go-netcup-scp):

```bash
netcup-scp auth login
kubectl create secret generic netcup-credentials -n netcup-system \
  --from-file=token=$HOME/.config/netcup-scp/token.json
```

### 2. Create a NetcupFailoverIP resource

Route a pair of failover IPs to any ready node:

```yaml
apiVersion: netcup.digilol.net/v1alpha1
kind: NetcupFailoverIP
metadata:
  name: primary
spec:
  ips:
    - "198.51.100.1/32"
    - "2001:db8::/64"
  credentialsSecret:
    name: netcup-credentials
```

`credentialsSecret` names a Secret in the controller's own namespace (`netcup-system` by default) — the controller's RBAC on Secrets is restricted to that namespace, so there is no namespace field to set.

Route to control-plane nodes only:

```yaml
apiVersion: netcup.digilol.net/v1alpha1
kind: NetcupFailoverIP
metadata:
  name: primary
spec:
  ips:
    - "198.51.100.1/32"
    - "2001:db8::/64"
  credentialsSecret:
    name: netcup-credentials
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/control-plane: ""
```

Route to worker nodes only:

```yaml
nodeSelector:
  matchExpressions:
    - key: node-role.kubernetes.io/control-plane
      operator: DoesNotExist
```

### 3. Check status

```bash
kubectl get netcupfailoverips
```

```
NAME        NODE                   ROUTED   AGE
primary     k8s-control-plane-1    True     5m
secondary   k8s-control-plane-2    True     5m
```

## Multiple IP Groups

Each `NetcupFailoverIP` resource is an independent group. IPs within a group are always routed to the same node. Groups are spread across different nodes automatically.

Groups may use different netcup accounts (one credentials Secret each). Placement is account-aware: a group only considers nodes whose `netcup.digilol.net/server-name` annotation names a server of *its* account, so clusters mixing nodes from several accounts need no extra labels or selectors.

Example — two independent groups, each with an IPv4 and IPv6 pair, using different netcup accounts:

```yaml
apiVersion: netcup.digilol.net/v1alpha1
kind: NetcupFailoverIP
metadata:
  name: group-a
spec:
  ips:
    - "198.51.100.1/32"
    - "2001:db8::/64"
  credentialsSecret:
    name: netcup-credentials-account1
---
apiVersion: netcup.digilol.net/v1alpha1
kind: NetcupFailoverIP
metadata:
  name: group-b
spec:
  ips:
    - "198.51.100.2/32"
    - "2001:db8::2/64"
  credentialsSecret:
    name: netcup-credentials-account2
```

## Security

- **Least-privilege RBAC** — Secret access (`get`/`create`/`update`) is a namespaced Role limited to the controller's namespace, and `credentialsSecret` is name-only, always resolved there; cluster-wide the controller can only watch nodes and its own CRD. A compromised controller cannot read or overwrite Secrets of other workloads.
- **Hardened pods** — distroless nonroot image, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp, no privilege escalation, pod anti-affinity across nodes.
- **NetworkPolicy** (`networkPolicy.enabled`, default on) — egress limited to DNS and TCP 443/6443, ingress to health probes; the metrics server is disabled entirely.
- **Node annotation protection** (`nodeAnnotationProtection.enabled`, default on, requires Kubernetes 1.30+) — a `ValidatingAdmissionPolicy` prevents kubelets from changing their own node's `netcup.digilol.net/server-name` annotation, so a compromised node cannot redirect failover IPs to another server.
- **No long-lived passwords** — OAuth2 device flow only; the token lives in a Secret and rotations are persisted automatically.
- **CI/supply chain** — `govulncheck` and `gosec` gate every PR, dependencies are updated by Dependabot, GitHub Actions are pinned to commit SHAs, and release images are signed with cosign (keyless) and published with provenance and SBOM attestations.

Verify a release image:

```bash
cosign verify ghcr.io/digilolnet/netcup-failover-controller:<tag> \
  --certificate-identity-regexp 'https://github.com/digilolnet/netcup-failover-controller/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Development

```bash
# Build
go build ./...

# Test
go test -race ./...

# Lint
golangci-lint run

# Security scan (same gates as CI)
govulncheck ./...
gosec -exclude-dir=agent ./...
```

The netcup endpoints can be overridden — e.g. for a mock API — via `NETCUP_SCP_API_URL` (SCP REST base) and `NETCUP_SCP_AUTH_URL` (OpenID Connect base), or the `netcup.apiUrl` / `netcup.authUrl` Helm values. Empty means the production endpoints.

The controller also runs out-of-cluster against your kubeconfig — useful for development, since leader election is pinned to the `netcup-system` namespace. Scale the in-cluster deployment to 0 first so it releases the lease:

```bash
kubectl -n netcup-system scale deploy netcup-failover-controller --replicas=0
go run .
```

Release images are built, signed, and pushed by CI on version tags — see below. Avoid pushing images manually; they would lack the cosign signature and provenance/SBOM attestations.

## Releases

Docker images are published to `ghcr.io/digilolnet/netcup-failover-controller` on every version tag. Push a tag to trigger a release:

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
```

Everything is derived from the tag — no version bumps in the repo are needed. Images are tagged as `X.Y.Z`, `X.Y`, `X`, and `latest`, signed with cosign, and published with provenance and SBOM attestations. The same workflow sets the chart's `version`/`appVersion` from the tag, packages it, and publishes it to the Helm repo on GitHub Pages.

## License

Apache 2.0
