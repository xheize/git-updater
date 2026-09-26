package gitManager

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestFingerprintSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := NewSQLiteJobStore(path)
	if err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "same", Image: "nginx", Tag: "v1"}
	if ok, err := s.EnqueueScoped(j, "update", ""); err != nil || !ok {
		t.Fatalf("enqueue: %v", err)
	}
	s.Close()
	s, err = NewSQLiteJobStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j.Timestamp = time.Now()
	if ok, err := s.EnqueueScoped(j, "update", ""); err != nil || ok {
		t.Fatalf("duplicate after restart: ok=%v err=%v", ok, err)
	}
	j.Tag = "v2"
	if _, err := s.EnqueueScoped(j, "update", ""); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
}

func TestLegacyJobIdentityMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := NewSQLiteJobStore(path)
	if err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "old", Image: "nginx", Tag: "v1"}
	if _, err := s.Enqueue(j); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"request_scope", "payload_hash", "outcome"} {
		if _, err := s.db.Exec("ALTER TABLE jobs DROP COLUMN " + col); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = NewSQLiteJobStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if info, found, err := s.Get(j.ID); err != nil || !found || info.Job.Tag != "v1" {
		t.Fatalf("legacy job lost: %+v %v", info, err)
	}
	if _, err := s.EnqueueScoped(j, "update", ""); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("unverified legacy duplicate accepted: %v", err)
	}
}
