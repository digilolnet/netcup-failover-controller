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
7. Every 5 minutes (configurable, see [Drift check](#drift-check)) the controller compares netcup's actual routing with the status and re-routes if something else changed it.

### What `Routed=True` means

`Routed=True` means netcup routes the group's IPs to the server of `status.currentNode`. It does **not** mean the node answers on those addresses: the controller never configures or probes them. A node that is `Ready` but cannot serve the address (see [Node Network Configuration](#node-network-configuration)) is still a valid target, and the resource will show `Routed=True` while the address is unreachable. Monitor the failover addresses from outside the cluster.

A node counts as healthy when its `Ready` condition is true, so a hard crash is only acted on once Kubernetes marks the node `NotReady`.

netcup rate-limits failover routing (10 requests per 5 minutes). When the limit is hit the controller records a `RateLimited` condition and retries after 5 minutes — IPs that already routed are never re-routed, so a large group converges across windows.

The controller runs with two replicas and leader election — one active, one standby. A pod that shuts down cleanly hands the lease over at once. If the active pod dies with its node, the standby takes over when the lease expires, about 15 seconds later (controller-runtime's defaults). That runs in parallel with Kubernetes noticing the dead node, which takes longer by default, so it should not add to the failover time; this case has not been tested.

## Prerequisites

- Each node must have the following annotation set:

| Annotation                       | Example value          |
| -------------------------------- | ---------------------- |
| `netcup.digilol.net/server-name` | `v1234567890123456789` |

## Node Network Configuration

The controller routes traffic to a node at the netcup network level. It does not configure addresses: every eligible node (not just the current holder) must carry the failover addresses itself, so that it can answer the moment routing switches to it.

### Recommended: failover addresses on loopback

Put the failover addresses of both families on `lo`, and nothing failover-related on the public interface:

```yaml
# patch.yaml
machine:
  network:
    interfaces:
      - interface: lo
        addresses:
          - 198.51.100.1/32 # IPv4 failover
          - 2001:db8::1/128 # each IPv6 failover address the node serves
```

netcup routes the failover IPs to the server, so they do not have to be on the public interface, and a single `/128` is enough for IPv6: the failover `/64` itself is not needed anywhere.

This layout was tested on three Talos control planes sharing one netcup segment. Both families answered from outside on the holder, and a clean reboot of the IPv6 holder re-routed the prefix in about 2 seconds with no failed probe out of 183 over three minutes. A failover of the IPv4 group in this layout and a hard crash of the holder have not been tested.

### The node's EUI-64 address must stay on the public interface

netcup's routers use the node's native EUI-64 address as the next hop for a failover IPv6 prefix. It has to be configured on the public interface, in addition to whatever native address the node otherwise uses:

```yaml
# patch.yaml
machine:
  network:
    interfaces:
      - interface: <interface-name> # e.g. eth0, ens3
        addresses:
          - 2001:db8:1:2:a8bb:ccff:fedd:eeff/64 # native EUI-64 host address
```

Derive it from the link-local address: if the link-local is `fe80::a8bb:ccff:fedd:eeff`, the EUI-64 address in the native prefix `2001:db8:1:2::/64` is `2001:db8:1:2:a8bb:ccff:fedd:eeff`.

> **A missing EUI-64 address does not fail immediately.** The routers keep forwarding on their cached neighbour entry, so a quick test passes. In one test the failover kept working for about 7 minutes after the address was removed; then a router's neighbour solicitations went unanswered and 22 of 50 outside probes failed until the address was restored.

The EUI-64 address is only needed as the failover next hop. The node's other native addresses (for example `::1`) are resolved individually by the routers and work without it.

### Why not the public interface

Earlier versions of this README put the failover `/64` on the public interface. That has two problems for IPv6 when the eligible nodes share a layer-2 segment, as netcup servers commonly do:

- **Duplicate address detection.** Every node carries the same address on the same segment, so the kernel on one node can see another node's copy and disable its own. `ip -6 addr` then shows the address as `dadfailed`, and which node loses depends on boot timing. When the controller routes the prefix to that node, the address is unreachable although the resource reports `Routed=True`.
- **Outbound source address.** The kernel may choose a failover address as the source for destinations whose prefix is closer to the failover range than to the node's own. Replies go to whichever node holds the prefix, so outbound IPv6 to those destinations fails on every other node. On `lo` this does not happen, because the kernel prefers addresses on the outgoing interface.

If you followed the earlier layout, both problems are latent in your cluster; move the addresses to `lo`. If the address must stay on the public interface, add it with duplicate detection disabled (`ip -6 addr add … nodad`), which avoids only the first problem.

IPv4 `/32` failover addresses are affected by neither, so moving them to `lo` is for consistency.

### Keeping a node out of rotation

The controller only looks at the node's `Ready` condition and the resource's `nodeSelector`. To exclude a node that is `Ready` but cannot serve the addresses, select on a label that you (or an agent on the node) control, and remove it from that node:

```yaml
nodeSelector:
  matchLabels:
    example.com/failover-capable: "true"
```

Removing the label is a Node change, so the controller moves the group to another eligible node immediately.

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

## Drift check

A routed group is otherwise left alone until its spec or a node changes. So that a change made elsewhere (the SCP panel, another tool) does not go unnoticed, the controller periodically lists the account's failover IPs and compares them with `status.currentNode`. If they differ it logs the affected IPs and routes them back. The check only reads, so it does not use up the routing rate limit; a re-route does. In a test, a prefix moved to another server through the SCP API was detected at the next check and routed back within 2 seconds.

The interval defaults to 5 minutes. Set the Helm value `resyncInterval` (or the `RESYNC_INTERVAL` environment variable) to a Go duration such as `10m`, or to `0` to disable the check.

`status.lastVerifiedTime` records when netcup's routing was last confirmed (shown as the `VERIFIED` column of `kubectl get netcupfailoverips -o wide`).

If the check itself fails (netcup API unavailable, credentials rejected), the `Routed` condition is left as it is, because the failure says nothing about where the IPs are routed. Instead a second condition, `Verified`, turns `False` with the reason, and the check is retried with backoff. `Routed=True` together with `Verified=False` therefore means "routed when last seen, but netcup cannot currently be asked" — a state in which a failover would also fail, so alert on it.

## Events

The controller records Kubernetes Events on the resource, so its actions outlive the pod log:

| Type | Reason | When |
| --- | --- | --- |
| Normal | `Routed` | The group was routed to a node (first placement, failover, or drift repair) |
| Warning | `DriftDetected` | netcup routed the IPs elsewhere than the status said |
| Warning | `RateLimited` | The routing rate limit was hit; retried after 5 minutes |
| Warning | `RoutingFailed`, `CredentialsError`, `ServerLookupError`, `VerificationFailed` | A routing attempt or drift check failed; retried with backoff |
| Warning | `NoEligibleNodes`, `NoAccountNodes`, `InvalidIP`, `InvalidNodeSelector` | The group cannot be placed until the nodes or the spec change |

`NetcupFailoverIP` is cluster-scoped, and Kubernetes stores events for cluster-scoped objects in the `default` namespace:

```bash
kubectl describe netcupfailoverip primary
kubectl get events -n default --field-selector involvedObject.kind=NetcupFailoverIP
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

The controller also runs out-of-cluster against your kubeconfig — useful for development: the leader-election lease and credentials Secrets are looked up in `netcup-system`, or in `CREDENTIALS_NAMESPACE` if the controller is installed elsewhere. Scale the in-cluster deployment to 0 first so it releases the lease:

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
