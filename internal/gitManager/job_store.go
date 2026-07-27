package gitManager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	jobStatusPending   = "pending"
	jobStatusRunning   = "running"
	jobStatusRetrying  = "retrying"
	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"
	maxJobAttempts     = 3
	jobRetryBaseDelay  = 5 * time.Second
)

// JobStore persists jobs so that accepted work is not lost when the process
// stops. It is intentionally backed by one SQLite connection because Git
// updates are also processed by a single worker.
type JobStore struct {
	db *sql.DB
}

type JobInfo struct {
	Job           Job        `json:"job"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"lastError,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
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
			action TEXT NOT NULL DEFAULT 'update',
			file TEXT NOT NULL,
			image TEXT NOT NULL,
			tag TEXT NOT NULL,
			timestamp_ns INTEGER NOT NULL,
			force INTEGER NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			created_at_ns INTEGER NOT NULL,
			updated_at_ns INTEGER NOT NULL,
			next_attempt_at_ns INTEGER NOT NULL DEFAULT 0
		)`,
		"CREATE INDEX IF NOT EXISTS jobs_status_created_at ON jobs(status, created_at_ns)",
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize job database: %w", err)
		}
	}
	return s.ensureColumns()
}

func (s *JobStore) ensureColumns() error {
	rows, err := s.db.Query("PRAGMA table_info(jobs)")
	if err != nil {
		return fmt.Errorf("inspect job database schema: %w", err)
	}

	columns := make(map[string]bool)
	for rows.Next() {
		var columnID, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&columnID, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("read job database schema: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate job database schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close job database schema inspection: %w", err)
	}
	if !columns["action"] {
		if _, err := s.db.Exec("ALTER TABLE jobs ADD COLUMN action TEXT NOT NULL DEFAULT 'update'"); err != nil {
			return fmt.Errorf("add job action column: %w", err)
		}
	}
	if !columns["next_attempt_at_ns"] {
		if _, err := s.db.Exec("ALTER TABLE jobs ADD COLUMN next_attempt_at_ns INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add job retry schedule column: %w", err)
		}
	}
	return nil
}

func (s *JobStore) Close() error {
	return s.db.Close()
}

func (s *JobStore) Enqueue(job Job) (bool, error) {
	if job.Action == "" {
		job.Action = JobActionUpdate
	}
	if job.Action != JobActionUpdate && job.Action != JobActionSync {
		return false, fmt.Errorf("unsupported job action %q", job.Action)
	}

	now := time.Now().UTC().UnixNano()
	result, err := s.db.Exec(
		`INSERT INTO jobs (id, action, file, image, tag, timestamp_ns, force, status, created_at_ns, updated_at_ns, next_attempt_at_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		job.ID,
		job.Action,
		job.File,
		job.Image,
		job.Tag,
		job.Timestamp.UTC().UnixNano(),
		boolToInt(job.Force),
		jobStatusPending,
		now,
		now,
		now,
	)
	if err != nil {
		return false, fmt.Errorf("persist job: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check persisted job: %w", err)
	}
	return inserted == 1, nil
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

	query := `SELECT id, action, file, image, tag, timestamp_ns, force FROM jobs WHERE `
	args := []any{}
	if id == "" {
		query += "status IN (?, ?) AND next_attempt_at_ns <= ?"
		args = append(args, jobStatusPending, jobStatusRetrying, time.Now().UTC().UnixNano())
	} else {
		query += "status = ? AND id = ?"
		args = append(args, jobStatusPending, id)
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
		 WHERE id = ? AND status IN (?, ?)`,
		jobStatusRunning,
		time.Now().UTC().UnixNano(),
		job.ID,
		jobStatusPending,
		jobStatusRetrying,
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
	var attempts int
	if err := s.db.QueryRow("SELECT attempts FROM jobs WHERE id = ? AND status = ?", id, jobStatusRunning).Scan(&attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("job %s is not running", id)
		}
		return fmt.Errorf("get failed job attempts: %w", err)
	}

	status := jobStatusFailed
	nextAttemptAt := int64(0)
	if attempts < maxJobAttempts {
		status = jobStatusRetrying
		nextAttemptAt = time.Now().UTC().Add(jobRetryDelay(attempts)).UnixNano()
	}
	return s.markWithRetry(id, status, failure, nextAttemptAt)
}

func (s *JobStore) mark(id, status, failure string) error {
	return s.markWithRetry(id, status, failure, 0)
}

func (s *JobStore) markWithRetry(id, status, failure string, nextAttemptAt int64) error {
	result, err := s.db.Exec(
		`UPDATE jobs SET status = ?, last_error = ?, updated_at_ns = ?, next_attempt_at_ns = ?
		 WHERE id = ? AND status = ?`,
		status,
		failure,
		time.Now().UTC().UnixNano(),
		nextAttemptAt,
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

func (s *JobStore) Get(id string) (JobInfo, bool, error) {
	row := s.db.QueryRow(`SELECT id, action, file, image, tag, timestamp_ns, force, status, attempts,
		COALESCE(last_error, ''), created_at_ns, updated_at_ns, next_attempt_at_ns FROM jobs WHERE id = ?`, id)
	info, err := scanJobInfo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return JobInfo{}, false, nil
	}
	if err != nil {
		return JobInfo{}, false, fmt.Errorf("get job: %w", err)
	}
	return info, true, nil
}

func (s *JobStore) Retry(id string) (Job, bool, error) {
	info, found, err := s.Get(id)
	if err != nil || !found {
		return Job{}, found, err
	}
	if info.Status != jobStatusFailed {
		return Job{}, true, fmt.Errorf("job %s is %s, only failed jobs can be retried", id, info.Status)
	}

	now := time.Now().UTC().UnixNano()
	result, err := s.db.Exec(`UPDATE jobs SET status = ?, attempts = 0, last_error = '', updated_at_ns = ?, next_attempt_at_ns = ?
		WHERE id = ? AND status = ?`, jobStatusPending, now, now, id, jobStatusFailed)
	if err != nil {
		return Job{}, true, fmt.Errorf("retry job: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Job{}, true, fmt.Errorf("check retried job: %w", err)
	}
	if changed != 1 {
		return Job{}, true, fmt.Errorf("job %s retry state changed", id)
	}
	return info.Job, true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var action string
	var timestampNS int64
	var force int
	if err := row.Scan(&job.ID, &action, &job.File, &job.Image, &job.Tag, &timestampNS, &force); err != nil {
		return Job{}, err
	}
	job.Action = JobAction(strings.TrimSpace(action))
	job.Timestamp = time.Unix(0, timestampNS).UTC()
	job.Force = force != 0
	return job, nil
}

func scanJobInfo(row rowScanner) (JobInfo, error) {
	var info JobInfo
	var action string
	var timestampNS, createdAtNS, updatedAtNS, nextAttemptAtNS int64
	var force int
	if err := row.Scan(&info.Job.ID, &action, &info.Job.File, &info.Job.Image, &info.Job.Tag, &timestampNS,
		&force, &info.Status, &info.Attempts, &info.LastError, &createdAtNS, &updatedAtNS, &nextAttemptAtNS); err != nil {
		return JobInfo{}, err
	}
	info.Job.Action = JobAction(strings.TrimSpace(action))
	info.Job.Timestamp = time.Unix(0, timestampNS).UTC()
	info.Job.Force = force != 0
	info.CreatedAt = time.Unix(0, createdAtNS).UTC()
	info.UpdatedAt = time.Unix(0, updatedAtNS).UTC()
	if nextAttemptAtNS > 0 {
		nextAttemptAt := time.Unix(0, nextAttemptAtNS).UTC()
		info.NextAttemptAt = &nextAttemptAt
	}
	return info, nil
}

func jobRetryDelay(attempt int) time.Duration {
	return jobRetryBaseDelay * time.Duration(1<<(attempt-1))
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
