# Contributing to aip-k8s

## Prerequisites

- Go 1.23+
- Docker
- [kind](https://kind.sigs.k8s.io/) (`~/go/bin/kind` or on `$PATH`)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [helm](https://helm.sh/docs/intro/install/) (for chart e2e only)

## Local development stack

Spin up the gateway and dashboard against your active cluster:

```bash
make local          # builds binaries, starts gateway (:8080) and dashboard (:8082)
make local-down     # stops both processes
make local-clean    # deletes all AIP objects from the cluster
```

`make local` uses `~/.kube/config`. Point it at any cluster — the `kind-aip-test`
cluster created by `make setup-test-e2e` works well.

## Unit and integration tests

```bash
make test           # envtest-based unit/integration tests (no cluster needed)
make lint           # golangci-lint
```

## Pre-merge e2e tests

These tests run Phases 1–6 against a Kind cluster with the controller deployed:

```bash
make test-e2e       # creates kind cluster, deploys controller, runs all e2e specs
```

Phase 6 (Gateway API tests) builds the gateway binary as a subprocess automatically —
no extra setup needed.

## Chart e2e tests

These tests validate the full Helm chart installation: controller, gateway, and dashboard
deployed together in-cluster.

### Running locally

```bash
# 1. Build images and load into Kind (one-time, or after code changes)
make chart-images

# 2. Install the chart and run the chart specs
make chart-e2e
```

`make chart-images` defaults to the `aip-test` Kind cluster and tags images as `local`.
Override with:

```bash
make chart-images CHART_KIND_CLUSTER=my-cluster CHART_IMAGE_TAG=dev
make chart-e2e    CHART_IMAGE_TAG=dev
```

`make chart-e2e` handles helm install, port-forwarding, test execution, and port-forward
cleanup automatically — including on test failure.

### How it works in CI

The `Chart E2E Tests` workflow triggers after `Publish Images` succeeds. It pulls the
published images from `ghcr.io`, loads them into a fresh Kind cluster, then calls:

```bash
make chart-e2e CHART_IMAGE_TAG=sha-<short-sha>
```

### Skipping chart tests in pre-merge runs

The chart specs in `test/e2e/helm_test.go` skip automatically when `GATEWAY_URL` is not
set, so they are never run during `make test-e2e`.

## Helm chart publishing

The Helm chart is published to `ghcr.io` as an OCI artifact automatically after every
merge to `main` (and on tagged releases) by the `Publish Images` workflow. No manual
steps are needed.

The published chart is available at:

```
oci://ghcr.io/ravisantoshgudimetla/aip-k8s/charts/aip-k8s
```

To publish a new chart version, bump `version` in `charts/aip-k8s/Chart.yaml` and merge
to `main`. The workflow packages and pushes the chart with that version tag automatically.
