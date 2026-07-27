package gitManager

import (
	"path/filepath"
	"testing"
	"time"
)

func TestJobStoreRecoversInterruptedJob(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "jobs.db")
	store, err := NewSQLiteJobStore(databasePath)
	if err != nil {
		t.Fatalf("create job store: %v", err)
	}

	job := Job{
		ID:        "job-1",
		Action:    JobActionUpdate,
		File:      "deployments/web.yaml",
		Image:     "nginx",
		Tag:       "1.25.4",
		Timestamp: time.Now().UTC().Round(0),
		Force:     true,
	}
	if _, err := store.Enqueue(job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := store.ClaimNext()
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if !ok || claimed.ID != job.ID {
		t.Fatalf("claimed job = %#v, ok = %v", claimed, ok)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close initial store: %v", err)
	}

	restartedStore, err := NewSQLiteJobStore(databasePath)
	if err != nil {
		t.Fatalf("reopen job store: %v", err)
	}
	defer restartedStore.Close()

	recovered, err := restartedStore.RecoverInterruptedJobs()
	if err != nil {
		t.Fatalf("recover interrupted jobs: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered jobs = %d, expected 1", recovered)
	}

	recoveredJob, ok, err := restartedStore.ClaimNext()
	if err != nil {
		t.Fatalf("claim recovered job: %v", err)
	}
	if !ok || recoveredJob != job {
		t.Fatalf("recovered job = %#v, expected %#v", recoveredJob, job)
	}
	if err := restartedStore.MarkSucceeded(recoveredJob.ID); err != nil {
		t.Fatalf("mark job succeeded: %v", err)
	}
	if _, ok, err := restartedStore.ClaimNext(); err != nil || ok {
		t.Fatalf("unexpected pending job after completion: ok=%v err=%v", ok, err)
	}
}

func TestJobStoreRetriesThenAllowsManualRetry(t *testing.T) {
	store, err := NewSQLiteJobStore(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("create job store: %v", err)
	}
	defer store.Close()

	job := Job{ID: "retry-job", Action: JobActionUpdate, Image: "nginx", Tag: "1.25", Timestamp: time.Now()}
	if _, err := store.Enqueue(job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	for attempt := 1; attempt <= maxJobAttempts; attempt++ {
		claimed, ok, err := store.ClaimNext()
		if err != nil || !ok {
			t.Fatalf("claim attempt %d: ok=%v err=%v", attempt, ok, err)
		}
		if err := store.MarkFailed(claimed.ID, "temporary failure"); err != nil {
			t.Fatalf("mark failure %d: %v", attempt, err)
		}

		info, found, err := store.Get(job.ID)
		if err != nil || !found {
			t.Fatalf("get failed job %d: found=%v err=%v", attempt, found, err)
		}
		if attempt < maxJobAttempts {
			if info.Status != jobStatusRetrying || info.NextAttemptAt == nil {
				t.Fatalf("job state after attempt %d = %#v", attempt, info)
			}
			if _, err := store.db.Exec("UPDATE jobs SET next_attempt_at_ns = 0 WHERE id = ?", job.ID); err != nil {
				t.Fatalf("make retry ready: %v", err)
			}
		} else if info.Status != jobStatusFailed {
			t.Fatalf("final job state = %q, expected %q", info.Status, jobStatusFailed)
		}
	}

	retried, found, err := store.Retry(job.ID)
	if err != nil || !found || retried.ID != job.ID {
		t.Fatalf("manual retry: job=%#v found=%v err=%v", retried, found, err)
	}
	info, found, err := store.Get(job.ID)
	if err != nil || !found || info.Status != jobStatusPending || info.Attempts != 0 {
		t.Fatalf("job state after manual retry = %#v found=%v err=%v", info, found, err)
	}
}
