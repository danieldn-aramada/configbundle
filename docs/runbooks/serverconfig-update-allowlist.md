# Update serverconfig OOB IP allowlist

The controller only reconciles ServerConfigs whose `oobIP` is in the allowlist.
Adding a new server requires updating the ConfigMap and restarting the controller.

## Steps

**1. Edit the ConfigMap** (comma-separated, no spaces):

```bash
kubectl patch cm configbundle-serverconfig-controller-config \
  -n configbundle-system \
  --type merge \
  -p '{"data":{"oobIPs":"10.20.21.41,10.20.21.44"}}'
```

**2. Restart the controller** (allowlist is read at startup, not watched live):

```bash
kubectl rollout restart deployment configbundle-serverconfig-controller -n configbundle-system
kubectl rollout status deployment configbundle-serverconfig-controller -n configbundle-system
```

**3. Verify the ServerConfig transitions out of Skipped:**

```bash
kubectl get serverconfig <name> -o jsonpath='{.status.phase}'
```

Expected: `Reconciled` (or `Observed` once the iDRAC observe pass runs).

## Notes

- The live ConfigMap is named `configbundle-serverconfig-controller-config` (kustomize namePrefix).
  A stale `serverconfig-controller-config` may exist from earlier deploys — it is not read by anything.
- Changes to the ConfigMap via `kubectl patch` are not persisted in the repo. Update
  `config/serverconfig/controller_config.yaml` and redeploy with `kubectl apply -k config/overlays/<cluster>`
  to make the change durable.
