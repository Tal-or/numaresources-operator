# NROP Pod Fingerprint Divergence on Workload Partitioned Clusters

## Problem Statement

On compact clusters where NROP targets the master pool with the default
`podsFingerprinting: EnabledExclusiveResources`, NUMA-aware scheduling is
**permanently blocked** on all nodes with the error:
`"invalid node topology data"`.

The root cause is a **pod fingerprint mismatch** between RTE and the
secondary scheduler, caused by OpenShift Workload Partitioning (WLP)
feature.

## Background: Workload Partitioning Resource Mutation

On WLP-enabled clusters, a two-stage mutation converts standard CPU requests
into an opaque extended resource:

1. **API server admission hook** (for regular OCP infra pods) or **kubelet** (for static
   pods) intercepts pod creation.
2. The mutation replaces `cpu: 400m` with
   `management.workload.openshift.io/cores: 400` (scaled ×1000) and injects
   `resources.workload.openshift.io/<container>: {"cpushares": 400}`
   annotations.
3. **CRI-O** reads the annotations and pins the container to the reserved
   (management) cpuset.

The result: WLP pods have **no native `cpu` requests** — only the
`management.workload.openshift.io/cores` extended resource. They are all
**Burstable** QoS.

## Background: Pod Fingerprinting

With `EnabledExclusiveResources`, both RTE and the scheduler compute a
fingerprint over pods that have "exclusive resources" on each node. If the
fingerprints match, the NRT data is considered fresh and scheduling proceeds.
If they diverge, the node is marked dirty and all NUMA-aware scheduling is
blocked.

## The Divergence

### RTE side (kubelet PodResources API → empty fingerprint)

RTE queries the **kubelet PodResources gRPC API** (`kubelet.sock`) to
enumerate pods and their resource allocations. It then filters using
`numalocality.Verify()`, which includes a pod only if it has:

- Exclusive CPU IDs (`len(cr.CpuIds) > 0`), or
- Memory with NUMA topology, or
- Devices with NUMA topology (`len(dev.DeviceIds) > 0 && IsPresent(dev.Topology)`)

**WLP pods are invisible to RTE** because `management.workload.openshift.io/cores`
is **not backed by a device plugin**. It is an extended resource advertised
directly by kubelet in node status. The device manager has no allocation
record for it, so `GetDevices()` returns nothing. These pods also have no
exclusive CPUs (they are Burstable, sharing the management cpuset via CRI-O
cpu shares). IOW they are completley ignored by the PodResources List call.

Result: **RTE computes an empty fingerprint** (zero exclusive-resource pods).

### Scheduler side (Kubernetes API → non-empty fingerprint)

The scheduler enumerates pods via the **Kubernetes API** (`podLister.List`)
and calls `AreExclusiveForPod(pod)` → `IsExclusive()` per resource:

```go
func IsExclusive(qos corev1.PodQOSClass, resource corev1.ResourceName,
    quantity resource.Quantity) bool {
    // treats ALL non-native resources as exclusive
    if !v1helper.IsNativeResource(resource) {
        return true
    }
    // ...native resource checks (QoS, integral CPU, etc.)
}
```

`management.workload.openshift.io/cores` is not a native resource →
`IsExclusive` returns `true`. The scheduler counts every WLP infra pod as
having exclusive resources.

Result: **scheduler computes a non-empty fingerprint** that includes all WLP
infra pods.

### Consequence

```
RTE fingerprint (NRT):          ""        (no exclusive pods seen)
Scheduler fingerprint (computed): "a1b2c3..." (WLP infra pods included)
→ ErrSignatureMismatch
→ node stays dirty, never flushed
→ filter returns Unschedulable("invalid node topology data")
→ ALL NUMA-aware scheduling permanently blocked
```

## Why the Two Sides See Different Things

| Aspect | RTE (PodResources API) | Scheduler (Kubernetes API) |
|--------|----------------------|--------------------------|
| Data source | kubelet gRPC socket | Pod specs from API server |
| Sees WLP extended resource? | No — no device plugin backs it | Yes — it's in the pod spec |
| Exclusivity check | NUMA topology presence | `!IsNativeResource()` → always exclusive |
| WLP pods counted? | **No** | **Yes** |

## Key Code Locations

| Component | File | Function |
|-----------|------|----------|
| RTE fingerprint filter | `resource-topology-exporter/pkg/podres/filter/numalocality/numalocality.go` | `Verify()` |
| RTE fingerprint build | `resource-topology-exporter/pkg/resourcemonitor/resourcemonitor.go` | `computePodFingerprintFromPodResources()` |
| Scheduler exclusivity check | `scheduler-plugins/pkg/noderesourcetopology/resourcerequests/exclusive.go` | `IsExclusive()` |
| Scheduler pod enumeration | `scheduler-plugins/pkg/noderesourcetopology/cache/overreserve.go` | `makeNodeToPodDataMap()` |
| Scheduler fingerprint compare | `scheduler-plugins/pkg/noderesourcetopology/cache/store.go` | `checkPodFingerprintForNode()` |
| Scheduler scheduling block | `scheduler-plugins/pkg/noderesourcetopology/filter.go` | `Filter()` — returns `Unschedulable` when `!info.Fresh` |
| Kubelet PodResources server | `k8s.io/kubernetes/pkg/kubelet/apis/podresources/server_v1.go` | `List()`, `getContainerResources()` |
| Kubelet device manager | `k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/manager.go` | `GetDevices()` — returns only device-plugin-backed allocations |

## Why This Only Manifests on Compact Clusters

On a **compact cluster** (3-node or SNO), master nodes double as worker
nodes — there is no separate worker pool. This creates the specific
conditions for the bug:

1. **NROP must target the master MachineConfigPool**, because that is the
   only pool where workloads can be scheduled (there are no dedicated
   workers).
2. **RTE runs on master nodes**, where the OpenShift control plane lives.
   These nodes are densely populated with WLP-mutated management pods:
   kube-apiserver, etcd, openshift-apiserver, oauth-server,
   cluster-version-operator, all operators, and dozens of DaemonSets — each
   with `management.workload.openshift.io/cores` in their pod spec.
3. The **scheduler sees all these WLP pods** on the master/worker nodes and
   counts them as exclusive. RTE sees none of them. The fingerprint diverges
   on every node.

On a **standard (non-compact) cluster**, this does not happen because:

- NROP targets the **worker MachineConfigPool**.
- RTE runs only on **dedicated worker nodes**.
- Worker nodes **do not host control plane pods** (kube-apiserver, etcd,
  openshift-apiserver, etc.) — those run exclusively on master nodes which
  NROP does not manage.
- While workers may still run some WLP DaemonSet pods (dns, multus, ovn,
  node-exporter, tuned), NROP is not configured on the master pool so the
  control-plane-heavy masters are out of scope entirely.

In short: compact clusters force NROP onto the same nodes as the full
OpenShift control plane, which is where the bulk of WLP pods reside.

### Why This Wasn't Caught Earlier (author assumetion)

WLP has been available for several releases, but until now it was
predominantly deployed on **Single Node OpenShift (SNO)** clusters. NROP is
**not useful on SNO** — there is only one node, so NUMA-aware *scheduling*
has no decisions to make (there is nowhere else to place a pod). As a result,
WLP and NROP were never running together on the same cluster.

The bug surfaces now because WLP is being adopted on **multi-node compact
clusters** (3-node), where NUMA-aware scheduling is meaningful and NROP is
deployed alongside WLP for the first time.

## Notes

- The kubelet PodResources API reports devices for **all QoS classes** (no
  Guaranteed-only filtering). The divergence is not about QoS — it is about
  the resource not being backed by a device plugin at all.
- The `IsExclusive()` comment acknowledges the assumption: *"until we reach
  better clarity we treat extended resources as devices"*. This assumption
  breaks with WLP's non-device extended resources.
- This is not an OCP-specific fix candidate in `scheduler-plugins` — WLP is
  an OpenShift feature that upstream `scheduler-plugins` has no awareness of.

  ## Possible Solutions (ordered by scope)

### Solution 1: Downstream-specific patch

d/s specific solution on <https://github.com/openshift-kni/scheduler-plugins> (first d/s patch)
to exclude WLP devices:

```go
   func IsExclusive(qos corev1.PodQOSClass, resource corev1.ResourceName,quantity resource.Quantity) bool {
      if IsWorkloadPartitionigDevice(resource) {
         return false
      }
      // treats ALL non-native resources as exclusive
      if !v1helper.IsNativeResource(resource) {
         return true
      }
      // ...native resource checks (QoS, integral CPU, etc.)
   }
   ```

#### Downsides

   1. Require drifting from the u/s code which in turn makes the RESYNC procedure more complex
   2. Address only WLP devices. future configuration of extended resources will break PFP calculation again.

### Solution 2: Upstream contribution

   u/s contribution to [kubernetes-sigs/scheduler-plugins](https://github.com/kubernetes-sigs/scheduler-plugins):
   add a configurable **excluded resource prefixes** list to the
   `NodeResourceTopologyCache` config.

#### Motivation (upstream-generic framing)

   The `IsExclusive()` function assumes all non-native resources are
   device-plugin-backed and therefore exclusive. This assumption is incorrect
   for **extended resources that are not backed by device plugins** — they are
   advertised directly by kubelet in node capacity but have no allocation
   records in kubelet's PodResources API. When the `EnabledExclusiveResources`
   fingerprinting method is used, the scheduler counts pods with these
   resources as exclusive (via the Kubernetes API), while RTE does not see
   them (via the PodResources API). The fingerprints permanently diverge,
   blocking NUMA-aware scheduling.

   **This is a generic problem (this is the key selling point) — any extended resource injected by an admission
   webhook or advertised directly by kubelet (without a device plugin) will
   trigger this divergence.**

#### Config change

   Add `ExcludedResourcePrefixes []string` to `NodeResourceTopologyCache`:

   ```go
   type NodeResourceTopologyCache struct {
       // ...existing fields...

       // ExcludedResourcePrefixes is a list of resource name prefixes that
       // should NOT be treated as exclusive resources when computing pod
       // fingerprints. Extended resources that are not backed by device
       // plugins are invisible to kubelet's PodResources API and would cause
       // a permanent fingerprint mismatch between RTE and the scheduler.
       // Example: ["management.workload.openshift.io/"]
       ExcludedResourcePrefixes []string `json:"excludedResourcePrefixes,omitempty"`
   }
   ```

   Files to change:

- `apis/config/types.go` — internal type
- `apis/config/v1/types.go` — versioned type with JSON tags
- `apis/config/validation/validation_pluginargs.go` — reject empty strings
- Run `hack/update-codegen.sh` for deepcopy + conversion
- No default needed (nil = no exclusions = backwards compatible)

#### Code change — ExclusionChecker

   Add an `ExclusionChecker` struct to `resourcerequests/exclusive.go`:

   ```go
   type ExclusionChecker struct {
       excludedPrefixes []string
   }

   func NewExclusionChecker(prefixes []string) *ExclusionChecker {
       if len(prefixes) == 0 { return nil }
       return &ExclusionChecker{excludedPrefixes: prefixes}
   }

   func (ec *ExclusionChecker) isExcluded(name corev1.ResourceName) bool {
       if ec == nil { return false }
       for _, p := range ec.excludedPrefixes {
           if strings.HasPrefix(string(name), p) { return true }
       }
       return false
   }
   ```

   Convert `IsExclusive`, `AreExclusiveForPod`, and `IncludeNonNative` to
   methods on `*ExclusionChecker` (nil receiver = current behavior):

   ```go
   func (ec *ExclusionChecker) IsExclusive(qos corev1.PodQOSClass,
       resource corev1.ResourceName, quantity resource.Quantity) bool {
       if ec.isExcluded(resource) {
           return false
       }
       if !v1helper.IsNativeResource(resource) {
           return true
       }
       // ...existing logic unchanged...
   }
   ```

#### Threading through callers

- **`cache/overreserve.go`** — `NewOverReserve()` already receives
     `cfg *NodeResourceTopologyCache`. Create `ExclusionChecker` from
     `cfg.ExcludedResourcePrefixes`, store on `OverReserve` struct. Pass to
     `makeNodeToPodDataMap()` to replace the direct
     `resourcerequests.AreExclusiveForPod(pod)` call (line 396).
- **`cache/foreign_pods.go`** — add a package-level `exclusionChecker`
     alongside the existing `onlyExclusiveResources` bool (same pattern).
     `IsForeignPod()` line 93 uses it via
     `exclusionChecker.AreExclusiveForPod(pod)`.
- **`filter.go`** — store checker on `TopologyMatch` struct, use in
     line 181 for `IncludeNonNative`.

#### User-facing scheduler config

   ```yaml
   apiVersion: kubescheduler.config.k8s.io/v1
   kind: KubeSchedulerConfiguration
   profiles:
     - pluginConfig:
         - name: NodeResourceTopologyMatch
           args:
             cache:
               excludedResourcePrefixes:
                 - "management.workload.openshift.io/"
   ```

#### Downsides

   The scheduler doesn't auto-detect non-device extended resources — the
   prefix list must be set in the manifest. Since NROP generates the
   scheduler config, it would need to detect WLP and inject the prefix.
   This requires changes in both **scheduler-plugins** (config + code) and
   **numaresources-operator** (manifest generation).
