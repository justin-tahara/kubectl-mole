# Recipes: Helm and Argo CD

mole doesn't know what Helm or Argo CD are — and doesn't need to. Both tools
label everything they manage, and mole fans out over labels. These recipes
are the verified shapes; every command here was run against a live cluster
before it was written down.

## Gate a Helm upgrade

Helm labels every workload it manages with the release name:

```
helm upgrade my-release ./chart -n prod
kubectl mole -n prod -l app.kubernetes.io/instance=my-release
```

One verdict for the whole release: exit 0 when every workload settled, exit 1
with the diagnosed cause when one cannot, exit 2 if something was still
legitimately progressing at the timeout — don't roll back on 2.

Compare `helm upgrade --wait`: it blocks until its timeout and then tells you
nothing about why. mole returns as soon as the failure is provable
(`--wedged-for`, default 30s) and names the cause:

```
workloads (namespace prod, selector app.kubernetes.io/instance=my-release): failed
reason: 1 of 1 workloads failed
pods: 0/1 ready, 1 failed (1 previous-revision still present)
failures:
  ImagePullBackOff: container web cannot pull image "registry.example.com/web:v1.9.3"
    chain: Deployment/my-release-web -> ReplicaSet/... -> Pod/...
```

The `previous-revision still present` note is the rollout wedged mid-surge:
the old pod is still serving while the new one cannot start. `helm rollback`
and re-run mole to confirm the rollback settled, too.

## Check what an Argo CD Application deployed

Argo CD applies `app.kubernetes.io/instance: <app-name>` to every resource an
Application manages (the default application instance label). So checking an
app is a fan-out over that label, across namespaces if the app spans them:

```
kubectl mole -A -l app.kubernetes.io/instance=my-app
```

**Do not point mole at the Application object itself.** Application status
(`status.health`, `status.sync`) follows Argo CD's own conventions, not
kstatus — there is no standard `Ready` condition and no
`observedGeneration`. Verified live: an Application stamped
`health: Degraded` reads `settled, 0/0 ready` under
`kubectl mole application/my-app`, because by kstatus rules there is nothing
wrong with it and it owns no pods. The label recipe watches the actual
workloads, diagnoses their pods, and cannot be fooled that way.

(If your Argo CD uses a different instance label —
`argocd.argoproj.io/instance` is the common alternative — substitute it.)

## Argo Rollouts

A Rollout is just a custom resource; the dynamic engine handles it like any
other, and diagnoses the pods underneath through the Rollout's own selector:

```
kubectl mole rollout/canary-demo -n prod --timeout 5m
```

Verified live against the argo-rollouts controller: a healthy canary settles;
a canary stuck on a bad image fails through `--wedged-for` with the chain
intact (`Rollout/canary-demo -> ReplicaSet/... -> Pod/...`) and the pull
error as evidence. Give `--timeout` room for your pause steps — a paused
canary is progressing, not failed, and mole will say so with exit 2.

Rollouts publishes `status.observedGeneration` as a string (a legacy of its
hash-based generations). kstatus rejects that type outright; mole normalizes
it — numeric strings keep the staleness guard, legacy hashes skip it — so
Rollouts gets a verdict instead of an error.

## Several clusters at once

An ApplicationSet (or a Helm release you deploy per cluster) rolls the same
app everywhere; checking it is the same fan-out with the cluster dimension
added:

```
kubectl mole --contexts us-east,us-west,eu-central -A -l app.kubernetes.io/instance=my-app
```

One verdict, one exit code, a per-context rollup, and identical causes
collapse across clusters — the same bad image in three clusters is one
failure entry, not three. A cluster that cannot be checked fails the verdict
instead of hiding. Details in [AGENTS.md](AGENTS.md#multi-cluster).

### Managed fleets: materialize, then sweep

`--contexts` needs the context entries in the local (merged) kubeconfig, but
credential rotation is already handled: per-context clients go through
kubeconfig `exec` plugins, so fresh tokens are fetched on every run. For
fleets where the kubeconfigs themselves are minted on demand (EKS, access
portals), materialize first, then sweep:

```
for c in $(aws eks list-clusters --query 'clusters[]' --output text); do
  aws eks update-kubeconfig --name "$c" --kubeconfig /tmp/fleet
done
KUBECONFIG=/tmp/fleet kubectl mole -A -l app.kubernetes.io/instance=my-app \
  --contexts ctx-a,ctx-b,ctx-c
```

Several kubeconfig files merge the standard way: `KUBECONFIG=a:b:c`. Fleets
whose context names rotate are tracked in
[issue #36](https://github.com/justin-tahara/kubectl-mole/issues/36).
