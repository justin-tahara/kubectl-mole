package signatures

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// TargetRef identifies the workload being diagnosed.
type TargetRef struct {
	Kind      string
	Namespace string
	Name      string
}

func (t TargetRef) String() string { return fmt.Sprintf("%s/%s", t.Kind, t.Name) }

// Report is the result of diagnosing an unsettled workload.
type Report struct {
	// Findings, workload-level first, then per pod sorted by pod name.
	Findings []Finding
	// Degraded lists reads that were denied and the analysis skipped as a
	// result. A verdict that silently omits analysis is confidently wrong.
	Degraded []string
}

// Diagnose walks the ownership chain of an unsettled workload and runs the
// signature catalogue over its current-revision pods. It degrades instead of
// failing: any denied read is recorded in Report.Degraded and analysis
// continues on what remains.
func Diagnose(ctx context.Context, cs kubernetes.Interface, target TargetRef, pods []*corev1.Pod) Report {
	d := &diagnoser{ctx: ctx, cs: cs, target: target}
	d.loadEvents()

	sc := &Context{
		PodEvents: d.podEvents,
		PVC:       d.pvc,
		PVCEvents: d.pvcEvents,
		CrashLogs: d.crashLogs,
		Node:      d.node,
	}

	var findings []Finding
	findings = append(findings, d.workloadFindings(sc)...)
	for _, p := range pods {
		for _, det := range podDetectors {
			f := det.detect(sc, p)
			if f == nil {
				continue
			}
			f.Chain = d.chain(p)
			f.Pod = p.Name
			findings = append(findings, *f)
			break
		}
	}
	return Report{Findings: dedupIdentical(findings), Degraded: d.degraded}
}

type diagnoser struct {
	ctx    context.Context
	cs     kubernetes.Interface
	target TargetRef

	events     []corev1.Event
	eventsOK   bool
	pvcCache   map[string]*corev1.PersistentVolumeClaim
	nodeCache  map[string]*corev1.Node
	degraded   []string
	logDenied  bool
	pvcDenied  bool
	nodeDenied bool
}

func (d *diagnoser) note(msg string) {
	for _, m := range d.degraded {
		if m == msg {
			return
		}
	}
	d.degraded = append(d.degraded, msg)
}

// loadEvents lists namespace events once and indexes them client-side.
func (d *diagnoser) loadEvents() {
	list, err := d.cs.CoreV1().Events(d.target.Namespace).List(d.ctx, metav1.ListOptions{})
	if err != nil {
		d.note(fmt.Sprintf("cannot list events in namespace %s: event evidence omitted, status fields only", d.target.Namespace))
		return
	}
	d.events = list.Items
	sort.SliceStable(d.events, func(i, j int) bool {
		return eventTime(d.events[i]).Time.Before(eventTime(d.events[j]).Time)
	})
	d.eventsOK = true
}

func eventTime(e corev1.Event) metav1.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp
	}
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return metav1.Time{Time: e.Series.LastObservedTime.Time}
	}
	return e.CreationTimestamp
}

func (d *diagnoser) podEvents(pod *corev1.Pod) []corev1.Event {
	return d.eventsFor("Pod", pod.Name, pod.UID)
}

func (d *diagnoser) pvcEvents(name string) []corev1.Event {
	return d.eventsFor("PersistentVolumeClaim", name, "")
}

func (d *diagnoser) eventsFor(kind, name string, uid types.UID) []corev1.Event {
	var out []corev1.Event
	for _, e := range d.events {
		if e.InvolvedObject.Kind != kind || e.InvolvedObject.Name != name {
			continue
		}
		if uid != "" && e.InvolvedObject.UID != "" && e.InvolvedObject.UID != uid {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (d *diagnoser) pvc(name string) *corev1.PersistentVolumeClaim {
	if d.pvcDenied {
		return nil
	}
	if d.pvcCache == nil {
		d.pvcCache = map[string]*corev1.PersistentVolumeClaim{}
	}
	if pvc, ok := d.pvcCache[name]; ok {
		return pvc
	}
	pvc, err := d.cs.CoreV1().PersistentVolumeClaims(d.target.Namespace).Get(d.ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			d.pvcDenied = true
			d.note("cannot read persistentvolumeclaims: PVC analysis skipped")
		}
		d.pvcCache[name] = nil
		return nil
	}
	d.pvcCache[name] = pvc
	return pvc
}

// node fetches one node by name — never a cluster-wide list; a workload's
// pods sit on few nodes. Denial degrades node-level analysis, it never fails
// the diagnosis.
func (d *diagnoser) node(name string) *corev1.Node {
	if d.nodeDenied {
		return nil
	}
	if d.nodeCache == nil {
		d.nodeCache = map[string]*corev1.Node{}
	}
	if n, ok := d.nodeCache[name]; ok {
		return n
	}
	n, err := d.cs.CoreV1().Nodes().Get(d.ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			d.nodeDenied = true
			d.note("cannot read nodes: node-level analysis skipped")
		}
		d.nodeCache[name] = nil
		return nil
	}
	d.nodeCache[name] = n
	return n
}

// crashLogs fetches the log tail of the container's most recent crashed
// instance. Which instance that is depends on the observed state: while the
// container sits terminated awaiting restart, the crash to read is the
// current instance — its predecessor is typically already garbage-collected
// (the kubelet retains only one dead container). While it is running or in
// backoff, the crash lives in the previous instance, and backoff is the one
// state where the kubelet refuses a non-previous read outright.
func (d *diagnoser) crashLogs(pod *corev1.Pod, status corev1.ContainerStatus) string {
	if d.logDenied {
		return ""
	}
	req := d.cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: status.Name,
		Previous:  status.State.Terminated == nil,
		TailLines: ptr.To(int64(20)),
	})
	raw, err := req.DoRaw(d.ctx)
	if err != nil {
		if apierrors.IsForbidden(err) {
			d.logDenied = true
			d.note("cannot read pods/log: log evidence omitted")
		}
		return ""
	}
	if body := string(raw); !kubeletLogError(body) {
		return body
	}
	return ""
}

// kubeletLogError recognizes the message the kubelet writes into a 200 log
// response when the requested container's logs are gone (rotated away, or
// the dead container was garbage-collected). It is kubelet text, not
// workload output, and must never be presented as log evidence.
func kubeletLogError(body string) bool {
	return strings.HasPrefix(body, "unable to retrieve container logs for ")
}

// chain is the ownership walk for one pod. The pod's controller ref supplies
// the middle link (the ReplicaSet for Deployments; for StatefulSets and
// DaemonSets the controller is the workload itself).
func (d *diagnoser) chain(p *corev1.Pod) []string {
	chain := []string{d.target.String()}
	if ref := metav1.GetControllerOf(p); ref != nil {
		link := fmt.Sprintf("%s/%s", ref.Kind, ref.Name)
		if link != chain[0] {
			chain = append(chain, link)
		}
	}
	return append(chain, "Pod/"+p.Name)
}

// workloadFindings covers causes that never reach a pod: pod creation being
// rejected by an admission webhook or a resource quota. The messages live on
// ReplicaSet conditions (for Deployments) and on FailedCreate events.
func (d *diagnoser) workloadFindings(_ *Context) []Finding {
	msgs := d.createFailureMessages()
	var out []Finding
	for _, m := range msgs {
		if f := matchAdmission(m); f != nil {
			f.Chain = []string{d.target.String()}
			out = append(out, *f)
			continue
		}
		if f := matchQuota(m); f != nil {
			f.Chain = []string{d.target.String()}
			out = append(out, *f)
		}
	}
	return out
}

func (d *diagnoser) createFailureMessages() []string {
	var msgs []string
	if d.target.Kind == "Deployment" {
		msgs = append(msgs, d.replicaSetFailures()...)
	}
	for _, e := range d.events {
		if e.Reason != "FailedCreate" {
			continue
		}
		k := e.InvolvedObject.Kind
		if k == "ReplicaSet" || k == d.target.Kind {
			msgs = append(msgs, e.Message)
		}
	}
	sort.Strings(msgs)
	return msgs
}

// replicaSetFailures reads ReplicaFailure conditions off the deployment's
// ReplicaSets, so quota and admission causes surface even when events are
// unreadable.
func (d *diagnoser) replicaSetFailures() []string {
	dep, err := d.cs.AppsV1().Deployments(d.target.Namespace).Get(d.ctx, d.target.Name, metav1.GetOptions{})
	if err != nil {
		d.note("cannot read the deployment: ReplicaSet failure conditions skipped")
		return nil
	}
	rss, err := d.cs.AppsV1().ReplicaSets(d.target.Namespace).List(d.ctx, metav1.ListOptions{})
	if err != nil {
		d.note("cannot list replicasets: ReplicaSet failure conditions skipped")
		return nil
	}
	var msgs []string
	for i := range rss.Items {
		rs := &rss.Items[i]
		ref := metav1.GetControllerOf(rs)
		if ref == nil || ref.UID != dep.UID {
			continue
		}
		for _, c := range rs.Status.Conditions {
			if c.Type == "ReplicaFailure" && c.Status == corev1.ConditionTrue {
				msgs = append(msgs, c.Message)
			}
		}
	}
	return msgs
}

// dedupIdentical removes findings that state the same fact about the same
// resource through two sources (an RS condition and its event). The anchor
// pod is part of the key: identical causes on different pods are distinct
// facts, and the collapse layer needs their multiplicity to count affected
// resources.
func dedupIdentical(in []Finding) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, f := range in {
		key := f.Signature + "\x00" + f.Cause + "\x00" + f.Pod
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

// authIndicated reports whether a pull error message implicates registry
// authentication.
func authIndicated(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"unauthorized", "authentication required", "pull access denied", "401", "403", "forbidden"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
