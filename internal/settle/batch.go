package settle

import (
	"fmt"
	"sort"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// jobSnapshot evaluates a Job. Success is completion, not held readiness:
// the snapshot is completionTerminal, so a Complete condition settles
// immediately and retry churn does not read as failure. kstatus maps
// Complete to Current and Failed (BackoffLimitExceeded, DeadlineExceeded)
// to Failed.
func (s *source) jobSnapshot() (snapshot, error) {
	j, err := s.jobs.Jobs(s.target.Namespace).Get(s.target.Name)
	if err != nil {
		return snapshot{}, notFoundOr(err, s.target)
	}
	return s.snapshotForJob(j)
}

// snapshotForJob is shared by Job targets and the CronJob delegation.
func (s *source) snapshotForJob(j *batchv1.Job) (snapshot, error) {
	if j.Spec.Suspend != nil && *j.Spec.Suspend {
		return snapshot{
			found:              true,
			completionTerminal: true,
			terminalFailure:    fmt.Sprintf("job %s is suspended (spec.suspend) and will not run until unsuspended", j.Name),
		}, nil
	}
	pods, err := s.selectorPods(j.Spec.Selector)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{
		found:              true,
		kstatus:            jobStatus(j),
		currentPods:        pods,
		completionTerminal: true,
	}, nil
}

// jobStatus computes a Job's status from its conditions directly. kstatus is
// deliberately not used here: it reports a started Job as Current while it
// is still running (an actively-running Job is the desired state from an
// apply-reconcile point of view), and mole's question is completion.
func jobStatus(j *batchv1.Job) kstatusResult {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			completions := int32(1)
			if j.Spec.Completions != nil {
				completions = *j.Spec.Completions
			}
			return kstatusResult{
				Status:  status.CurrentStatus,
				Message: fmt.Sprintf("job completed (succeeded %d/%d)", j.Status.Succeeded, completions),
			}
		case batchv1.JobFailed:
			msg := c.Message
			if c.Reason != "" && msg != "" {
				msg = c.Reason + ": " + msg
			} else if c.Reason != "" {
				msg = c.Reason
			}
			return kstatusResult{Status: status.FailedStatus, Message: msg}
		}
	}
	if j.Status.StartTime == nil {
		return kstatusResult{Status: status.InProgressStatus, Message: "job has not started"}
	}
	return kstatusResult{
		Status:  status.InProgressStatus,
		Message: fmt.Sprintf("job in progress (active %d, succeeded %d, failed %d)", j.Status.Active, j.Status.Succeeded, j.Status.Failed),
	}
}

// cronJobSnapshot evaluates a CronJob through its most recent scheduled Job.
// Nothing scheduled yet is progress, not failure — the schedule simply has
// not fired — and a suspended CronJob will never fire, which is terminal.
func (s *source) cronJobSnapshot() (snapshot, error) {
	cj, err := s.cronJobs.CronJobs(s.target.Namespace).Get(s.target.Name)
	if err != nil {
		return snapshot{}, notFoundOr(err, s.target)
	}
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		return snapshot{
			found:              true,
			completionTerminal: true,
			terminalFailure:    fmt.Sprintf("cronjob %s is suspended (spec.suspend) and will not schedule until unsuspended", cj.Name),
		}, nil
	}
	newest, err := s.newestOwnedJob(cj)
	if err != nil {
		return snapshot{}, err
	}
	if newest == nil {
		return snapshot{
			found:              true,
			completionTerminal: true,
			kstatus: kstatusResult{
				Status:  status.InProgressStatus,
				Message: fmt.Sprintf("no job scheduled yet (schedule %q)", cj.Spec.Schedule),
			},
		}, nil
	}
	snap, err := s.snapshotForJob(newest)
	if err != nil {
		return snapshot{}, err
	}
	snap.kstatus.Message = fmt.Sprintf("job %s: %s", newest.Name, snap.kstatus.Message)
	return snap, nil
}

// newestOwnedJob returns the CronJob's most recently created Job, or nil
// when none exists.
func (s *source) newestOwnedJob(cj *batchv1.CronJob) (*batchv1.Job, error) {
	all, err := s.jobs.Jobs(cj.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	var owned []*batchv1.Job
	for _, j := range all {
		if ref := metav1.GetControllerOf(j); ref != nil && ref.UID == cj.UID {
			owned = append(owned, j)
		}
	}
	if len(owned) == 0 {
		return nil, nil
	}
	sort.Slice(owned, func(i, k int) bool {
		if !owned[i].CreationTimestamp.Equal(&owned[k].CreationTimestamp) {
			return owned[i].CreationTimestamp.Before(&owned[k].CreationTimestamp)
		}
		return owned[i].Name < owned[k].Name
	})
	return owned[len(owned)-1], nil
}

// podSnapshot evaluates a bare Pod. A Succeeded pod is terminally settled (a
// run-once pod that finished is done, not flapping); a Running pod holds the
// ordinary stability window.
func (s *source) podSnapshot() (snapshot, error) {
	p, err := s.pods.Pods(s.target.Namespace).Get(s.target.Name)
	if err != nil {
		return snapshot{}, notFoundOr(err, s.target)
	}
	ks, err := computeKstatus(p, "v1", "Pod")
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{
		found:              true,
		kstatus:            ks,
		currentPods:        []*corev1.Pod{p},
		completionTerminal: p.Status.Phase == corev1.PodSucceeded,
	}, nil
}
