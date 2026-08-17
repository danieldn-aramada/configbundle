# Edge deploy: all controllers

All four controllers (cb-controller, sc-controller, bc-controller, cb-bundler
as sidecar) ship from this repo. A single `kubectl apply -k` deploys CRDs +
three Deployments + per-controller RBAC.

## Endpoints (after deploy)

| URL | Purpose |
|---|---|
| `http://configbundle-controller.configbundle-system:8095/dispatch` | orb POSTs OCI layer bodies here |
| `http://serverconfig-controller-metrics.configbundle-system:8093/metrics` | Prometheus scrape target |
| `http://backupconfig-controller-metrics.configbundle-system:8094/metrics` | Prometheus scrape target |

## Prereqs

```bash
# Namespace
kubectl create namespace configbundle-system

# iDRAC credentials Secret (NEVER commit the password)
kubectl create secret generic idrac-credentials \
  -n configbundle-system \
  --from-literal=username=root \
  --from-literal=password='<actual-password>'
```

Also required (assumed already in place):

- **orb** Service reachable as `orb:8010` from inside the namespace.
- **ACR pull secret** in `configbundle-system` (site-specific Deployment patch).

## Set cluster name (one-time, per site)

sc-controller must know which cluster it is deployed on so it only reconciles its own ServerConfigs. Set `clusterName` in the site overlay (e.g. `config/overlays/dev-main/sc_cluster_patch.yaml`) to the exact `KubernetesCluster.name` value from Orbital — this is what cb-controller stamps on the `serverconfig.armada.ai/cluster-name` label. Default is `"UNSET"`, which matches no ServerConfig.

## Tune the allowlist (one-time, per site)

Edit `config/serverconfig/controller_config.yaml` in this repo:

```yaml
oobIPs: "10.20.21.44,..."           # iDRAC IPs the controller may PATCH
fields: "sshEnabled,racadmEnabled,ipmiEnabled"
```

`oobIPs` is the blast-radius control — CRs targeting other IPs are silently
skipped.

For a live update without rebuilding, see [serverconfig-update-allowlist.md](serverconfig-update-allowlist.md).

## Set image versions

All images are pinned in `config/default/kustomization.yaml`:

```yaml
images:
- name: controller
  newName: armadaeksatest.azurecr.io/configbundle-controller
  newTag: v0.0.5
- name: serverconfig
  newName: armadaeksatest.azurecr.io/serverconfig-controller
  newTag: v0.0.3
- name: backupconfig
  newName: armadaeksatest.azurecr.io/backupconfig-controller
  newTag: v0.0.5
```

Bump `newTag` here before deploy. See [tag-and-release.md](tag-and-release.md)
for how to cut new tags and push images.

## Deploy

```bash
kubectl apply -k ~/armada/configbundle/config/default
```

Idempotent — same command upgrades.

## Verify

```bash
kubectl -n configbundle-system get pods,svc
kubectl get crd | grep armada.ai

# Serverconfig drift-detection log line
kubectl -n configbundle-system logs deploy/configbundle-serverconfig-controller --tail=20 | grep drift

# Metrics scrape
kubectl -n configbundle-system port-forward svc/configbundle-serverconfig-controller-metrics 8093 &
curl -s http://localhost:8093/metrics | grep configbundle_
```

## Teardown

```bash
kubectl delete -k ~/armada/configbundle/config/default
# Optional: kubectl delete namespace configbundle-system
```
