# The signature catalogue

Every failure entry in a verdict names one signature from this catalogue —
a stable enum, safe to match on in automation. This page is the operator's
reference: what fires each signature, and what to do about it.

## How signatures are assigned

- **One finding per pod, first match wins.** Detectors run in priority
  order: deeper causes come before the symptoms they produce, so a dead
  node reports `NodeNotReady`, not the forty crash-loops it caused, and an
  OOM kill reports `OOMKilled`, not the crash-loop it turned into.
- **Init containers say so in the cause** ("init container migrate is
  crash-looping"); the signature is the mechanism either way.
- **Identical causes collapse.** Findings sharing a signature and cause
  merge into one entry: `affected` counts the resources, `examples` names
  up to three.
- **Old-revision pods get exactly one question** — are they wedging the
  rollout? Only `NodeNotReady` and `PodStuckTerminating` fire on them;
  their other symptoms are history.
- **Workload-level signatures** (`AdmissionRejected`, `QuotaExceeded`)
  come from the controller's own failure to create pods; there may be no
  pod at all to inspect.

## Pod-level signatures, in priority order

### NodeNotReady

The pod is scheduled to a node whose `Ready` condition is false or
unknown. The cause names the node, never the pod, so every pod on that
node collapses into one entry. Fix the node (or let the cloud provider
replace it); the pods follow.

### PodStuckTerminating

The pod has a deletion timestamp and has overstayed its grace period by
more than 30 seconds — almost always a finalizer that is not being
removed, and the evidence quotes the finalizers. Find the controller
responsible for the finalizer; deleting it by hand works but hides the
broken controller.

### PVCPending

The pod is blocked on a PersistentVolumeClaim stuck in `Pending`. The
evidence carries the claim's events — usually a missing StorageClass, an
exhausted provisioner, or a WaitForFirstConsumer claim whose pod cannot
schedule. Fix the claim, not the pod.

### PodSandboxFailed

The kubelet could not create the pod sandbox (`FailedCreatePodSandBox`
events): CNI failures, exhausted pod CIDRs, a broken container runtime.
This is node or cluster infrastructure, not the workload.

### VolumeMountFailed

`FailedMount` or `FailedAttachVolume` events while the pod is still
waiting to start: a missing Secret or ConfigMap behind a volume, a CSI
attach timeout, or a volume still attached to another node
(multi-attach). The event message names the volume.

### PodUnschedulable

The scheduler reports `PodScheduled: false`, and the cause carries its
predicate message — insufficient CPU or memory, unsatisfiable affinity,
taints without tolerations. Note that this signature is *not* an early-
failure state: capacity can arrive, so an unschedulable pod holds the
verdict at progressing until the timeout.

### PodEvicted

The pod is in phase `Failed` with reason `Evicted`: node pressure or an
exceeded ephemeral-storage limit. The eviction message in the evidence
says which resource ran out.

### OOMKilled

A container was killed by the OOM killer (current or last termination
state). The fix is a memory limit raise or a leak fix — restarting buys
minutes. Fires before `CrashLoopBackOff` deliberately: the OOM is the
cause, the crash loop is the consequence.

### ConfigMissing

`CreateContainerConfigError`: a ConfigMap or Secret referenced by `env`
or `envFrom` does not exist or lacks the referenced key. The kubelet's
message names the missing object. (The same mistake behind a *volume*
surfaces as `VolumeMountFailed`.)

### ContainerStartFailed

The runtime accepted the image but could not start the container:
`StartError`, `ContainerCannotRun`, `RunContainerError`,
`CreateContainerError` — typically a missing executable, a bad
entrypoint, or a runtime-level failure. The full runtime error is the
cause.

### CrashLoopBackOff

The container starts, exits, and is restarting under backoff — including
the between-backoff moment where the state briefly reads as running. The
evidence carries the crash log tail, which is where the actual reason
lives. Fires only after deeper causes (OOM, config, start errors) have
had their chance to claim the pod.

### ContainerFailed

The pod is in phase `Failed` with a nonzero exit code and no restart
coming — the shape of a Job retry pod. For Jobs these are progress, not
failure; the signature appears in diagnosis so the exit code and logs are
in the verdict when the Job finally exhausts its backoff.

### ImagePullBackOff

`ImagePullBackOff` or `ErrImagePull`: the image does not exist, the tag
is wrong, or the node lacks pull credentials. The registry's own error
text is in the evidence. This is a wedge state — with `--wedged-for` it
fails early rather than consuming the timeout.

### ProbeFailing

The pod runs but never becomes Ready, and `Unhealthy` probe events say
why. Check the probe's threshold against the app's real startup time
before assuming the app is broken.

## Workload-level signatures

### AdmissionRejected

Pod creation was denied by an admission webhook or a
ValidatingAdmissionPolicy — read from the ReplicaSet's `FailedCreate`
condition, because the pod never existed. The cause names the admitter
and quotes its denial message.

### QuotaExceeded

Pod creation was refused by a ResourceQuota. The cause names the quota
and the requested resource that exceeded it.

## When no signature fires

A verdict can be `failed` or `progressing` with an empty `failures[]` —
mole never invents a cause it cannot evidence. The `reason` field still
states what the settle engine saw (for example, a rollout that never
converged), and `degraded[]` lists any reads RBAC denied that might have
hidden the cause.
