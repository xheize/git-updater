package gitManager

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xheize/git-updater/internal/controller"
)

func (s *JobStore) initController() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS change_sets (id TEXT PRIMARY KEY, payload TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS controller_binding (slot INTEGER PRIMARY KEY CHECK(slot=1), identity TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS job_baselines (id TEXT PRIMARY KEY, revision TEXT NOT NULL)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}
func (s *JobStore) bindIdentity(identity string) error {
	if _, err := s.db.Exec(`INSERT INTO controller_binding(slot,identity) VALUES(1,?) ON CONFLICT DO NOTHING`, identity); err != nil {
		return err
	}
	var bound string
	if err := s.db.QueryRow(`SELECT identity FROM controller_binding WHERE slot=1`).Scan(&bound); err != nil {
		return err
	}
	if bound != identity {
		return fmt.Errorf("%w: database is bound to another repository/ref", controller.ErrConflict)
	}
	return nil
}
func (s *JobStore) savePlan(p controller.Plan, insert bool) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if insert {
		_, err = s.db.Exec(`INSERT INTO change_sets(id,payload,created_at) VALUES(?,?,?)`, p.ID, string(data), p.CreatedAt)
		return err
	}
	r, err := s.db.Exec(`UPDATE change_sets SET payload=? WHERE id=?`, string(data), p.ID)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return controller.ErrNotFound
	}
	return err
}
func (s *JobStore) GetPlan(id string) (controller.Plan, error) {
	var p controller.Plan
	var data string
	err := s.db.QueryRow(`SELECT payload FROM change_sets WHERE id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return p, controller.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(data), &p)
	return p, err
}
func (s *JobStore) ListPlans(limit, offset int) ([]controller.Plan, error) {
	rows, err := s.db.Query(`SELECT payload FROM change_sets ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []controller.Plan{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var p controller.Plan
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *JobStore) ListJobs(limit, offset int) ([]JobInfo, error) {
	rows, err := s.db.Query(`SELECT id,action,file,image,tag,timestamp_ns,force,status,attempts,COALESCE(last_error,''),created_at_ns,updated_at_ns,next_attempt_at_ns,outcome FROM jobs ORDER BY created_at_ns DESC,id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobInfo{}
	for rows.Next() {
		p, err := scanJobInfo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
