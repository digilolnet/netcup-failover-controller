# netcup-failover-controller

A Kubernetes controller that automatically routes [netcup](https://www.netcup.de) failover IPs to healthy cluster nodes via the netcup SOAP API.

## Overview

The controller watches `NetcupFailoverIP` custom resources. Each resource defines a group of failover IPs (e.g. one IPv4 + one IPv6) that are always routed to the same node. When a node becomes unhealthy, the controller re-routes to a different healthy node automatically.

Multiple `NetcupFailoverIP` resources are spread across different nodes for bandwidth splitting — the controller avoids placing two groups on the same node when alternatives exist.

## How It Works

1. A `NetcupFailoverIP` resource is created with a list of IPs and a reference to a Secret containing netcup credentials.
2. The controller lists healthy nodes (optionally filtered by a label selector).
3. It picks a node deterministically (`hash(resource name) % eligible nodes`), preferring nodes not already hosting another failover IP group.
4. It fetches the node's MAC address from the netcup SOAP API and calls `ChangeIPRouting` to route each IP there.
5. Status is updated with the current node and a `Routed` condition.
6. Node changes trigger re-evaluation for all resources.

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
helm install netcup-failover-controller netcup/netcup-failover-controller --version 1.0.0
```

### Manual

```bash
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
kubectl apply -f config/manager/
```


## Usage

### 1. Create a credentials Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: netcup-credentials
  namespace: netcup-system
stringData:
  loginName: "12345"
  password: "your-netcup-password"
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
    namespace: netcup-system
```

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
    namespace: netcup-system
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
    namespace: netcup-system
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
    namespace: netcup-system
```

## Development

```bash
# Build
go build ./...

# Test
go test ./...

# Lint
golangci-lint run

# Build and push Docker image (multi-arch) — build binaries first, then push
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/manager-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/manager-linux-arm64 .
docker buildx build --platform linux/amd64,linux/arm64 --tag ghcr.io/digilolnet/netcup-failover-controller:v1.0.0 --push .
```

## Releases

Docker images are published to `ghcr.io/digilolnet/netcup-failover-controller` on every version tag. Push a tag to trigger a release:

```bash
git tag v1.0.0
git push origin v1.0.0
```

Images are tagged as `v1.0.0`, `v1.0`, and `v1`.

## License

Apache 2.0
