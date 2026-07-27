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
