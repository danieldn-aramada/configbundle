# Orbital CMDB Reference

> **When to load this file:** Read this before working on the Orbital GraphQL data model, the bundler's query logic, the ConfigBundle manifest YAML structure, the local override/divergence model, or the K8s controller pattern that governs actuation.

---

## Overview

Orbital is the cloud CMDB — it stores **design intent**, not observed hardware state. Its data model is a Dgraph graph database exposed via GraphQL at `POST /api/v1/graphql`. The configbundle bundler queries this schema to build the ConfigBundle manifest. This file captures the entity model, the manifest format the bundler produces, and the architectural decisions (from the SDD and CMDB Architectural Proposal) that govern how configbundle interacts with Orbital and how the edge applies the result.

**On bootstrapping:** Orbital data is initially seeded from Redfish/iDRAC scans and discovery tooling. Once in Orbital, those values are the authoritative desired configuration — not a running snapshot of current hardware state. A `firmwareVersion` field in Orbital means "we intend this version", even if it was originally populated by scanning the hardware at day 1.

---

## Key decisions

- **Air-gapped first** — all components must run without cloud connectivity; this is a primary design constraint, not a secondary one. Eliminated a significant portion of otherwise viable tooling choices.
- **Dgraph + GraphQL for the CMDB** — graph database for traversal-heavy queries (impact analysis, dependency expansion, physical-to-logical mapping). GraphQL enables flexible, client-driven queries. Do not suggest migrating to a relational model.
- **Netbox continues for network topology** — Netbox is the source of truth for network infrastructure. All other config items are in Orbital CMDB. Do not move network data to Dgraph.
- **K8s controller pattern for actuation; CMDB is not in the reconciliation path** — Orbital CMDB stores intent. ConfigBundle controller and X Config Controllers actuate locally on the Galleon. Orbital has no role after a ConfigBundle CR lands. Do not design any path where Orbital drives actuation or is consulted during reconciliation.
- **ConfigBundle is the top-level orchestration object** — it aggregates the full config set for a target Galleon and decomposes into domain-specific child CRs (ServerConfig, ClusterConfig, NetworkConfig, etc.) via ownerReferences.
- **Local overrides via SSA field managers — ConfigBundle CR only** — field manager `local:<admin-id>` for edge admin overrides on the ConfigBundle CR; `configbundle-controller` for bundle-owned fields. Overrides exist only at the ConfigBundle CR level. Child CRs (ServerConfig, ClusterConfig, etc.) are derived state and are always faithfully overwritten by the Decomposition Reconciler. Do not build a custom versioning or conflict-resolution system; SSA already tracks field ownership.
- **Conflict resolution principle: cloud intent wins for desired state; conflicts surface but do not auto-resolve** — three cloud admin actions: Force (apply cloud intent over local override), Accept (incorporate local value into next bundle), Ignore (acknowledge divergence; leave as-is).

---

## Orbital GraphQL entity model

The configbundle bundler queries Orbital GraphQL using this schema. All types implement the `ConfigItem` interface (fields: `id`, `namespace`, `name`, `createdBy`, `createdAt`, `updatedBy`, `updatedAt`, `version`).

### Top-level types relevant to bundler queries

**`DataCenter`** — the primary query anchor (filter by `name`)
```graphql
type DataCenter implements ConfigItem {
  location: String
  owner: String
  type: String
  chassis: [Chassis]
  coolingSystems: [CoolingSystem]
  clusters: [KubernetesCluster]
  powerSystems: [PowerSystem]
  servers: [Server]
  structuralComponents: [StructuralComponent]
  spareComponents: [SpareComponent]
}
```

**`Server`**
```graphql
type Server implements ConfigItem {
  model: String
  surveyTag: String
  biosSettings: BiosSettings
  chassis: [Chassis]
  dataCenter: DataCenter!
  ethernetInterfaces: [EthernetInterface]
  idracSettings: IdracSettings
  kubernetesNode: KubernetesNode
  memory: [Memory]
  powerSupplies: [PowerSupply]
  processors: [Processor]
  serverConfigurationProfile: ServerConfigurationProfile
  storageControllers: [StorageController]
  systemSettings: SystemSettings
}
```

**`KubernetesCluster`**
```graphql
type KubernetesCluster implements ConfigItem {
  provider: String  # eksa
  dataCenter: [DataCenter]
  nodes: [KubernetesNode]
  clusterConfig: [ClusterConfig]
  applicationConfig: [ApplicationConfig]
}
```

**`ClusterConfig`** — contains EKS-A cluster YAML and hardware CSV
```graphql
type ClusterConfig implements ConfigItem {
  eksaClusterYaml: String
  eksaHardwareCsv: String
  hash: String
  cluster: KubernetesCluster!
}
```

### Server sub-types

| Type | Key fields |
|---|---|
| `SystemSettings` | `firstBootDevice`, `fanSpeedOffset`, `thermalProfileOptimization` |
| `IdracSettings` | `usbManagementPortEnabled`, `osToIdracPassThroughEnabled` |
| `BiosSettings` | `bootMode`, `bootSequenceRetry`, `genericUsbBoot`, `pxeDevice` |
| `PxeDevice` | `enabled`, `interface`, `protocol`, `vlanEnabled`, `vlanID`, `vlanPriority` |
| `ServerConfigurationProfile` | `json` (full iDRAC config profile as JSON string, ~500 KB) |
| `EthernetInterface` | `ethernetInterfaceType`, `permanentMACAddress`, `speedMbps` |
| `StorageController` | `storageDevices: [StorageDevice]` |
| `StorageDevice` | `capacityBytes`, `manufacturer`, `model`, `serialNumber`, `wwn` |
| `StorageVolume` | `capacityBytes`, `manufacturer`, `model`, `serialNumber` |
| `Processor` | `manufacturer`, `model`, `maxSpeedMHz`, `operatingSpeedMHz`, `socket`, `totalCores` |
| `Memory` | `capacityMiB`, `manufacturer`, `model`, `memoryDeviceType`, `memoryType` |
| `PowerSupply` | `serialNumber`, `firmwareVersion`, `powerCapacityWatts`, `lineInputVoltageType` |

### Datacenter sub-types

| Type | Key fields |
|---|---|
| `PowerSystem` | `type` (GENERATOR, UPS, PDU) |
| `CoolingSystem` | `type` (HVAC, LIQUID_COOLING) |
| `Chassis` | `type`, `dataCenter`, `servers: [Server]` |
| `StructuralComponent` | `type` (DOOR, PIPE, FRAME, PANEL) |

### Standard bundler query pattern

```graphql
query ConfigBundleFields($dc: String!) {
  queryDataCenter(filter: { name: { eq: $dc } }) {
    name
    servers {
      name
      surveyTag
      biosSettings { bootMode bootSequenceRetry }
      systemSettings { firstBootDevice thermalProfileOptimization }
      idracSettings { usbManagementPortEnabled }
      # add fields needed by domain controllers
    }
    clusters {
      provider
      clusterConfig { eksaClusterYaml eksaHardwareCsv }
    }
  }
}
```

---

## ConfigBundle manifest format

The bundler produces a YAML manifest (`application/vnd.armada.configbundle.manifest.v1+yaml`) encoding the desired state for the target Galleon. The ConfigBundle controller on the Galleon reads this and decomposes it into child CRs.

```yaml
apiVersion: armada.ai/v1
kind: ConfigBundle
metadata:
  name: colo-galleon
spec:
  servers:
    - name: server-01
      biosProfile: performance
      powerLimit: "500w"
      pxeBoot: enabled
      raidConfig: non-raid
    - name: server-02
      biosProfile: balanced
      powerLimit: "400w"
  clusters:
    - type: workload
      kubernetesVersion: "1.28"
      nodeCount: 7
      storageClass: ceph-rbd
    - type: management
      kubernetesVersion: "1.28"
      nodeCount: 3
  network: ...
```

The ConfigBundle controller decomposes this into `ServerConfig`, `ClusterConfig`, `X_Config` child CRs, each owned via ownerReferences. When a new bundle arrives, child CRs are created, updated, or deleted to match.

**Generated `ServerConfig` child CR:**
```yaml
apiVersion: armada.ai/v1
kind: ServerConfig
metadata:
  name: server-01
  ownerReferences:
    - apiVersion: armada.ai/v1
      kind: ConfigBundle
      name: colo-galleon
      controller: true
      blockOwnerDeletion: true
  managedFields:
    - manager: config-bundle-controller
      operation: Apply
      fieldsV1:
        f:spec:
          f:biosProfile: {}
          f:powerLimit: {}
          f:pxeBoot: {}
          f:raidConfig: {}
spec:
  biosProfile: performance
  powerLimit: "500w"
  pxeBoot: enabled
  raidConfig: non-raid
```

---

## Local override / divergence model

**Override flow:**

1. Edge admin edits a field via local CLI: `kubectl apply --server-side --field-manager=local:<admin-id> ...`
2. Kubernetes records ownership in `managedFields` — field X on CR Y is now owned by `local:<admin-id>`
3. Next bundle reconciliation: `config-bundle-controller` applies via SSA; leaves non-owned fields untouched. Override persists automatically.
4. Edge agent tracks `managedFields` on ConfigBundle CR; builds divergence report (which fields overridden, by whom, since when)
5. Report published to external location (S3/NFS) on schedule or on demand

**Cloud admin resolution actions:**

| Action | What happens |
|---|---|
| **Reject** | Orbital intent unchanged. Bundle includes `spec.takeover[]` for the field. cb-controller's `processTakeover` force-reclaims the field from `local:admin`; `reconcileLocalClaims` strips local:admin's SSA claim automatically. Original cloud value wins. No edge admin action required. |
| **Accept** | Orbital mutates intent in DGraph to match the local override value first. Bundle then includes `spec.takeover[]` for the field — same mechanism as Reject. cb-controller force-reclaims with the new cloud value (= the accepted local value). `reconcileLocalClaims` strips local:admin's claim automatically. No edge admin action required. |
| **Ignore** | Orbital intent unchanged. Bundle includes `spec.ignored[]` for the field; cb-controller bows out unconditionally. local:admin retains sole ownership. Divergence persists and remains visible. |

**Divergence report contains:** field-level divergence between active ConfigBundle intent and observed state — which fields are locally overridden, by whom, and since when. Reports are observability artifacts only; they do not drive actuation.

---

## Gotchas

- **`ServerConfigurationProfile.json` is large** — a full iDRAC server config profile exported as JSON is ~15k lines, ~500 KB uncompressed. Do not include this in the ConfigBundle manifest unless specifically needed; it will bloat every bundle.
- **Media type discrepancy between SDD §4.8 and integration doc** — The SDD §4.8 lists `application/vnd.armada.configbundle.data.v1+json` and `application/vnd.armada.configbundle.schema.v1+json` for all three layers. The `configbundle-integration.md` contract uses `application/vnd.orbital.subgraph.data.v1+gzip` and `application/vnd.orbital.subgraph.schema.v1+gzip` for the orbital-produced layers. **Treat `configbundle-integration.md` as the source of truth** for the current integration design; §4.8 may reflect an earlier design where configbundle produced all three layers.
- **CMDB is never in the reconciliation path** — the ConfigBundle controller must not query Orbital during reconciliation. It reads only from the ConfigBundle CR in etcd.
- **Edge CMDB (local Dgraph) is rebuildable** — deleting and rebuilding the Edge CMDB must be safe at any time. The authoritative local state is the CRs in etcd and the last received bundle, not the edge Dgraph instance.
- **Dgraph audit logging is enterprise-gated** — do not rely on Dgraph's native audit log for application audit requirements. Logging must be implemented at the service layer.

---

## External references

- [SDD: DCIM & CMDB for Galleon Digital Twin in Atlas](../../SDD%20DCIM%20%26%20CMBD%20for%20Galleon%20Digital%20Twin%20in%20Atlas%20%283%29.pdf)
- [CMDB Architectural Proposal (Sedar)](../../CMDB_Architectural_Proposal.docx)
- [Enricher integration design](../../configbundle-integration.md)

---

## Domain file maintenance

Update this file when:
- The Orbital GraphQL schema adds or removes types/fields used by the bundler
- The ConfigBundle manifest format is finalized or extended (new domains)
- The divergence report format or transport is decided
- The local override workflow changes materially

Updates must be in the same PR as the code change that prompted them.
