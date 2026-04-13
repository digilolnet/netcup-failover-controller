# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

A Kubernetes controller-runtime operator that routes netcup failover IPs to healthy cluster nodes via the netcup SOAP API. Uses a `NetcupFailoverIP` CRD (`netcup.digilol.net/v1alpha1`).

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
internal/controller/   # reconciler
internal/netcup/       # SOAP client
config/crd/            # CRD manifest
config/rbac/           # ClusterRole, ClusterRoleBinding, ServiceAccount
config/manager/        # Deployment manifest
config/samples/        # example NetcupFailoverIP resources
```

## Key design decisions

- IPs in the CRD spec use CIDR notation (`198.51.100.1/32`, `2001:db8::/64`); the controller splits IP and prefix before calling the SOAP API
- Credentials are per-resource via `credentialsSecret` referencing a K8s Secret with `loginName` and `password` keys
- `NetcupFailoverIP` is **cluster-scoped**; the `credentialsSecret` field still carries a namespace because `SecretReference` requires one
- `corev1.Secret` is excluded from the controller-runtime cache — credential reads always go directly to the API server
- Node selection is deterministic (`hash(resource.name) % eligible nodes`), preferring nodes not already hosting another failover IP group
- Controller watches `Node` objects; any node change re-enqueues all `NetcupFailoverIP` resources
- No retry logic in the SOAP client — the controller retries up to 10× with a 3s interval; netcup rate-limits routing changes to once per 5 minutes and the controller requeues after 5 minutes on that error
- Two replicas with leader election; must run on control-plane nodes
- Each node requires annotation `netcup.digilol.net/server-name`; the MAC address is fetched live from the SOAP API
- In the SOAP client, `password` is XML-escaped; `loginName` and other string fields are interpolated directly into the XML body
