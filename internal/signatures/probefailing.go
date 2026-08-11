package signatures

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// detectProbeFailing fires on unready pods whose probe failures are visible
// in events. It names the probe kind, the container, and the probed endpoint
// — the three things needed to reproduce the failure by hand. Requires
// events; with events unreadable this detector stays silent and the
// degradation is reported by the orchestrator.
func detectProbeFailing(c *Context, pod *corev1.Pod) *Finding {
	if podIsReady(pod) {
		return nil
	}
	e := latestEventWhere(c.PodEvents(pod), func(e corev1.Event) bool {
		return e.Reason == "Unhealthy"
	})
	if e == nil {
		return nil
	}
	kind := "readiness"
	switch {
	case strings.HasPrefix(e.Message, "Liveness"):
		kind = "liveness"
	case strings.HasPrefix(e.Message, "Startup"):
		kind = "startup"
	}
	container := containerFromFieldPath(e.InvolvedObject.FieldPath)
	cause := fmt.Sprintf("%s probe failing", kind)
	if container != "" {
		cause += " for container " + container
		if desc := describeProbe(pod, container, kind); desc != "" {
			cause += " (" + desc + ")"
		}
	}
	return &Finding{
		Signature: "ProbeFailing",
		Cause:     cause,
		Evidence:  []Evidence{{Source: "event", Text: clip(e.Message, maxEventEvidence)}},
	}
}

func podIsReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// containerFromFieldPath extracts the container name from an event's
// involvedObject.fieldPath, e.g. "spec.containers{main}".
func containerFromFieldPath(fp string) string {
	start := strings.IndexByte(fp, '{')
	end := strings.IndexByte(fp, '}')
	if start < 0 || end <= start {
		return ""
	}
	return fp[start+1 : end]
}

func describeProbe(pod *corev1.Pod, container, kind string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != container {
			continue
		}
		var p *corev1.Probe
		switch kind {
		case "liveness":
			p = c.LivenessProbe
		case "startup":
			p = c.StartupProbe
		default:
			p = c.ReadinessProbe
		}
		if p == nil {
			return ""
		}
		switch {
		case p.HTTPGet != nil:
			return fmt.Sprintf("GET %s on port %s", p.HTTPGet.Path, p.HTTPGet.Port.String())
		case p.TCPSocket != nil:
			return "TCP port " + p.TCPSocket.Port.String()
		case p.Exec != nil:
			return "exec " + strings.Join(p.Exec.Command, " ")
		case p.GRPC != nil:
			return fmt.Sprintf("gRPC on port %d", p.GRPC.Port)
		}
	}
	return ""
}
