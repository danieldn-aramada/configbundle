# Tag and release

configbundle ships four independent binaries. Each gets its own tag
namespace + image; you version them independently.

## Tag namespaces

| Component | Tag prefix | Image name |
|---|---|---|
| cb-controller | `controller/v*` | `armadaeksatest.azurecr.io/configbundle-controller` |
| cb-bundler | `bundler/v*` | `armadaeksatest.azurecr.io/configbundle-bundler` |
| sc-controller | `serverconfig/v*` | `armadaeksatest.azurecr.io/serverconfig-controller` |
| bc-controller | `backupconfig/v*` | `armadaeksatest.azurecr.io/backupconfig-controller` |

Versions are derived automatically via `git describe --tags --match '<prefix>/v*'`.
Dirty working tree → `-dirty` suffix. No matching tag anywhere →
`<prefix>/v0.0.0-dev` fallback.

Current tags on this repo:

```bash
git tag -l | sort
```

## Release one component

Cut a semver tag at HEAD:

```bash
git tag controller/v0.0.4
```

Log in to ACR (once per session):

```bash
az acr login --name armadaeksatest
```

Build and push:

```bash
make push-controller
```

The Makefile derives `CONTROLLER_VERSION=v0.0.4` from your new tag. If
you need to override without tagging:

```bash
make push-controller CONTROLLER_VERSION=v0.0.4
```

## Release everything at once

```bash
git tag controller/v0.0.4
git tag bundler/v0.0.5
git tag serverconfig/v0.0.3
git tag backupconfig/v0.0.2

az acr login --name armadaeksatest
make push-all
```

`make push-all` chains all four `push-*` targets. Shared Dockerfile builder
layer is cached across the four builds, so second-through-fourth pushes
finish in seconds.

## Semver conventions

- **Patch bump** (`v0.0.X` → `v0.0.X+1`) — bug fixes, small non-breaking
  additions
- **Minor bump** (`v0.X.0` → `v0.X+1.0`) — new features, non-breaking
- **Major bump** (`vX.0.0` → `vX+1.0.0`) — breaking changes (schema break,
  API removal, CronJob shape change). Coordinate with orbital.

Prototype status: everything's `v0.0.x`. First minor bump waits for MVP
lock-in.

## Where images end up

`armadaeksatest.azurecr.io/<image-name>:<version>`. Both edge clusters
(colo-dev-main and future galleons) pull from this ACR via a scheduled
Zot sync (not directly — see the [orbital repo](../../../orbital) for the
sync architecture).

## Verify a pushed image

```bash
az acr repository show-tags \
  --name armadaeksatest \
  --repository configbundle-controller \
  --output table | head -20
```

## After pushing — deploy to edge

Push is only halfway. The edge cluster's Deployment YAML references a
specific tag; bumping ACR alone doesn't trigger a rollout. Continue with
[`deploy-to-edge.md`](deploy-to-edge.md).

## Bundler is deployed via orbital

`configbundle-bundler` runs as a sidecar in the Orbital pod on AKS
(cloud-side), not on the edge. After pushing a new bundler tag, coordinate
with whoever manages the orbital deploy to roll it out — the config lives
in the orbital repo's kustomize.

The other three (`cb-controller`, `sc-controller`, `bc-controller`) deploy
to the edge via this repo's kustomize. See
[`deploy-to-edge.md`](deploy-to-edge.md).
