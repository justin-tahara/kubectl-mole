package settle

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
)

// TestJobStatusRunningIsNotCurrent pins the reason jobStatus exists: kstatus
// reports a started, still-running Job as Current, and mole's question is
// completion.
func TestJobStatusRunningIsNotCurrent(t *testing.T) {
	j := &batchv1.Job{
		Status: batchv1.JobStatus{
			StartTime: &metav1.Time{},
			Active:    1,
		},
	}
	ks := jobStatus(j)
	if ks.Status != status.InProgressStatus {
		t.Fatalf("a running job is in progress, got %s (%s)", ks.Status, ks.Message)
	}
}

func TestJobStatusComplete(t *testing.T) {
	j := &batchv1.Job{
		Spec: batchv1.JobSpec{Completions: ptr.To(int32(2))},
		Status: batchv1.JobStatus{
			Succeeded:  2,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		},
	}
	ks := jobStatus(j)
	if ks.Status != status.CurrentStatus || !strings.Contains(ks.Message, "succeeded 2/2") {
		t.Fatalf("want completed 2/2, got %s (%s)", ks.Status, ks.Message)
	}
}

func TestJobStatusFailedCarriesReason(t *testing.T) {
	j := &batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
			}},
		},
	}
	ks := jobStatus(j)
	if ks.Status != status.FailedStatus || !strings.Contains(ks.Message, "BackoffLimitExceeded") {
		t.Fatalf("want failed with the condition reason, got %s (%s)", ks.Status, ks.Message)
	}
}

func TestJobStatusNotStarted(t *testing.T) {
	ks := jobStatus(&batchv1.Job{})
	if ks.Status != status.InProgressStatus || !strings.Contains(ks.Message, "not started") {
		t.Fatalf("want in progress / not started, got %s (%s)", ks.Status, ks.Message)
	}
}
