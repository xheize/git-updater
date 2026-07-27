package gitManager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	jobStatusPending   = "pending"
	jobStatusRunning   = "running"
	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"
)

// JobStore persists jobs so that accepted work is not lost when the process
// stops. It is intentionally backed by one SQLite connection because Git
// updates are also processed by a single worker.
type JobStore struct {
	db *sql.DB
}

func NewSQLiteJobStore(databasePath string) (*JobStore, error) {
	if databasePath == "" {
		return nil, errors.New("job database path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(databasePath), 0750); err != nil {
		return nil, fmt.Errorf("create job database directory: %w", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("open job database: %w", err)
	}
	db.SetMaxOpenConns(1)

	store := &JobStore{db: db}
	if err := store.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *JobStore) initialize() error {
	for _, statement := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
		`CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			file TEXT NOT NULL,
			image TEXT NOT NULL,
			tag TEXT NOT NULL,
			timestamp_ns INTEGER NOT NULL,
			force INTEGER NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			created_at_ns INTEGER NOT NULL,
			updated_at_ns INTEGER NOT NULL
		)`,
		"CREATE INDEX IF NOT EXISTS jobs_status_created_at ON jobs(status, created_at_ns)",
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize job database: %w", err)
		}
	}
	return nil
}

func (s *JobStore) Close() error {
	return s.db.Close()
}

func (s *JobStore) Enqueue(job Job) error {
	now := time.Now().UTC().UnixNano()
	_, err := s.db.Exec(
		`INSERT INTO jobs (id, file, image, tag, timestamp_ns, force, status, created_at_ns, updated_at_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID,
		job.File,
		job.Image,
		job.Tag,
		job.Timestamp.UTC().UnixNano(),
		boolToInt(job.Force),
		jobStatusPending,
		now,
		now,
	)
	if err != nil {
		return fmt.Errorf("persist job: %w", err)
	}
	return nil
}

// RecoverInterruptedJobs makes jobs claimed by a process that stopped before
// recording an outcome available to the next worker startup.
func (s *JobStore) RecoverInterruptedJobs() (int64, error) {
	result, err := s.db.Exec(
		`UPDATE jobs
		 SET status = ?, last_error = ?, updated_at_ns = ?
		 WHERE status = ?`,
		jobStatusPending,
		"worker interrupted before completion",
		time.Now().UTC().UnixNano(),
		jobStatusRunning,
	)
	if err != nil {
		return 0, fmt.Errorf("recover interrupted jobs: %w", err)
	}
	return result.RowsAffected()
}

func (s *JobStore) ClaimNext() (Job, bool, error) {
	return s.claim("")
}

func (s *JobStore) Claim(id string) (Job, bool, error) {
	return s.claim(id)
}

func (s *JobStore) claim(id string) (Job, bool, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, fmt.Errorf("begin job claim: %w", err)
	}
	defer tx.Rollback()

	query := `SELECT id, file, image, tag, timestamp_ns, force
		FROM jobs WHERE status = ?`
	args := []any{jobStatusPending}
	if id != "" {
		query += " AND id = ?"
		args = append(args, id)
	}
	query += " ORDER BY created_at_ns, id LIMIT 1"

	job, err := scanJob(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return Job{}, false, fmt.Errorf("commit empty job claim: %w", err)
		}
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("select pending job: %w", err)
	}

	result, err := tx.ExecContext(
		ctx,
		`UPDATE jobs SET status = ?, attempts = attempts + 1, updated_at_ns = ?
		 WHERE id = ? AND status = ?`,
		jobStatusRunning,
		time.Now().UTC().UnixNano(),
		job.ID,
		jobStatusPending,
	)
	if err != nil {
		return Job{}, false, fmt.Errorf("claim job: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Job{}, false, fmt.Errorf("check claimed job: %w", err)
	}
	if changed != 1 {
		return Job{}, false, errors.New("job claim lost")
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, fmt.Errorf("commit job claim: %w", err)
	}
	return job, true, nil
}

func (s *JobStore) MarkSucceeded(id string) error {
	return s.mark(id, jobStatusSucceeded, "")
}

func (s *JobStore) MarkFailed(id, failure string) error {
	return s.mark(id, jobStatusFailed, failure)
}

func (s *JobStore) mark(id, status, failure string) error {
	result, err := s.db.Exec(
		`UPDATE jobs SET status = ?, last_error = ?, updated_at_ns = ?
		 WHERE id = ? AND status = ?`,
		status,
		failure,
		time.Now().UTC().UnixNano(),
		id,
		jobStatusRunning,
	)
	if err != nil {
		return fmt.Errorf("mark job %s: %w", status, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check marked job: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("job %s is not running", id)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var timestampNS int64
	var force int
	if err := row.Scan(&job.ID, &job.File, &job.Image, &job.Tag, &timestampNS, &force); err != nil {
		return Job{}, err
	}
	job.Timestamp = time.Unix(0, timestampNS).UTC()
	job.Force = force != 0
	return job, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
