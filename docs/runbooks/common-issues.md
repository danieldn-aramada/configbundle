# Common issues

## "field not declared in schema" on apply

cb-controller fails to apply a ConfigBundle or ServerConfig with errors like:
```
.spec.servers[...].kubernetesNode.clusterName: field not declared in schema
```

**Cause:** The CRD on the target cluster is older than the code. Happens when a new field is added to `api/v1/` but the CRD wasn't updated on the cluster before deploying the new controller.

**Fix:** Apply the updated CRDs to every cluster that runs cb-controller:
```bash
# Edge cluster
kubectl --context <cluster> apply -k ~/armada/configbundle/config/default

# Local minikube
make install
```

`kubectl apply -k config/default/` is idempotent and always applies CRDs before rolling controllers — use it as the standard deploy path, not `make install` alone.

---

## Labels missing on ServerConfigs after bundle import

`kubectl get serverconfig --show-labels` shows `<none>` on all rows even after a fresh import.

**Cause 1: Bundler is old.** Labels are stamped by cb-controller from data in the manifest. If the bundler image predates the label changes, the manifest won't carry the cluster fields and no label is stamped.

**Check:**
```bash
kubectl --context <cluster> get configbundle <name> \
  -o jsonpath='{.spec.servers[?(@.kubernetesNode)].kubernetesNode}' | jq .
```
If `clusterName` is absent, the bundler is old. Push a new bundler image, redeploy the orbital pod, and re-import.

**Cause 2: cb-controller is old.** Check the deployed image:
```bash
kubectl --context <cluster> get deployment -n configbundle-system \
  configbundle-controller -o jsonpath='{.spec.template.spec.containers[0].image}'
```
If it's not the expected version, the kustomize apply didn't roll out. Check pod events.

---

## OCI artifact shows "unverified" signature after push

Orb's tag list shows `Signature: unverified` for the new tag.

**Cause:** Zot hasn't finished syncing the cosign signature from ACR yet. This is transient — signatures land a few seconds after the manifest.

**Fix:** Wait 30–60 seconds and check again. Do not retry the import until the signature shows verified; orb will reject unverified artifacts.

---

## `kubectl get sc` returns StorageClasses instead of ServerConfigs

`sc` is a built-in alias for `StorageClass` in some kubectl contexts.

**Fix:** Use the full resource name or specify the API group:
```bash
kubectl get serverconfig --show-labels
kubectl get serverconfigs.armada.ai --show-labels
```

Always include `--context <cluster>` when targeting an edge cluster to avoid accidentally hitting the wrong cluster.
