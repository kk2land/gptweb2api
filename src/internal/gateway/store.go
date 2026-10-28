package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }
type taskRecord struct {
	Task            Task      `json:"task"`
	TokenHash       string    `json:"token_hash"`
	CompletionHash  string    `json:"completion_hash"`
	CompletedAt     time.Time `json:"completed_at"`
	Started         bool      `json:"started"`
	Binding         Binding   `json:"binding"`
	ScriptHash      string    `json:"script_hash"`
	LeaseUntil      time.Time `json:"lease_until"`
	Calibration     string    `json:"calibration"`
	DotSubscription string    `json:"dot_subscription"`
	DirectoryID     string    `json:"directory_id"`
}

func encodeTask(t *Task) ([]byte, error) {
	return json.Marshal(taskRecord{Task: *t, TokenHash: t.TokenHash, CompletionHash: t.CompletionHash, CompletedAt: t.CompletedAt, Started: t.Started, Binding: t.Binding, ScriptHash: t.ScriptHash, LeaseUntil: t.LeaseUntil, Calibration: t.Calibration, DotSubscription: t.DotSubscription, DirectoryID: t.DirectoryID})
}
func decodeTask(b []byte) (*Task, error) {
	var r taskRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	t := r.Task
	t.TokenHash, t.CompletionHash, t.CompletedAt, t.Started, t.Binding, t.ScriptHash = r.TokenHash, r.CompletionHash, r.CompletedAt, r.Started, r.Binding, r.ScriptHash
	t.LeaseUntil, t.Calibration, t.DotSubscription = r.LeaseUntil, r.Calibration, r.DotSubscription
	t.DirectoryID = r.DirectoryID
	return &t, nil
}

func OpenStore(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,state TEXT NOT NULL,updated INTEGER NOT NULL,payload BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS tasks_state ON tasks(state,updated);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,updated INTEGER NOT NULL,payload BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS tool_calls(task_id TEXT NOT NULL,operation_id TEXT NOT NULL,arguments_hash TEXT NOT NULL,state TEXT NOT NULL,result BLOB,updated INTEGER NOT NULL,PRIMARY KEY(task_id,operation_id),FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS subscriptions(id TEXT PRIMARY KEY,expires INTEGER NOT NULL,payload BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS event_outbox(id TEXT PRIMARY KEY,subscription_id TEXT NOT NULL,task_id TEXT NOT NULL,payload BLOB NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL,delivered INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS runtime_state(name TEXT PRIMARY KEY,payload BLOB NOT NULL);
PRAGMA user_version=1;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

// The task and updated session history commit together, before completion is
// acknowledged. Callers serialize transitions with Manager.mu.
func (s *Store) save(t *Task, session *Session) error {
	b, err := encodeTask(t)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO tasks VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,updated=excluded.updated,payload=excluded.payload`, t.ID, t.State, t.Updated.UnixMilli(), b)
	if err != nil {
		return err
	}
	if session != nil {
		b, err = json.Marshal(session)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO sessions VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET updated=excluded.updated,payload=excluded.payload`, session.ID, time.Now().UnixMilli(), b)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) loadTask(id string) (*Task, error) {
	var b []byte
	if err := s.db.QueryRow(`SELECT payload FROM tasks WHERE id=?`, id).Scan(&b); err != nil {
		return nil, err
	}
	return decodeTask(b)
}
func (s *Store) session(id string) (*Session, error) {
	var b []byte
	if err := s.db.QueryRow(`SELECT payload FROM sessions WHERE id=?`, id).Scan(&b); err != nil {
		return nil, err
	}
	var session Session
	if err := json.Unmarshal(b, &session); err != nil {
		return nil, err
	}
	return &session, nil
}
func (s *Store) recoverTasks() ([]*Task, error) {
	rows, err := s.db.Query(`SELECT payload FROM tasks WHERE state NOT IN ('completed','failed','cancelled','expired') ORDER BY updated,id`)
	if err != nil {
		return nil, err
	}
	var tasks []*Task
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		t, err := decodeTask(b)
		if err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Calls started before a crash are deliberately never replayed.
	_, err = s.db.Exec(`UPDATE tool_calls SET state='unknown' WHERE state='running'`)
	return tasks, err
}
func (s *Store) cleanup() error {
	cutoff := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	_, err := s.db.Exec(`DELETE FROM tasks WHERE state IN ('completed','failed','cancelled','expired') AND updated<?`, cutoff)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM sessions WHERE updated<? AND id NOT IN (SELECT COALESCE(json_extract(payload,'$.task.request.x_gptweb2api.session_id'),'') FROM tasks WHERE state NOT IN ('completed','failed','cancelled','expired'))`, cutoff)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM event_outbox WHERE next_attempt<? AND (delivered=1 OR task_id NOT IN(SELECT id FROM tasks)); DELETE FROM subscriptions WHERE expires<?`, cutoff, time.Now().UnixMilli())
	return err
}
func (s *Store) toolStart(taskID, opID, argsHash string) (json.RawMessage, error) {
	var hash, state string
	var result []byte
	err := s.db.QueryRow(`SELECT arguments_hash,state,result FROM tool_calls WHERE task_id=? AND operation_id=?`, taskID, opID).Scan(&hash, &state, &result)
	if err == nil {
		if hash != argsHash {
			return nil, fault("operation_conflict", "operation_id was already used with different arguments", 409)
		}
		if state != "done" {
			return nil, fault("execution_unknown", "previous operation is running or its outcome is unknown; inspect before retrying", 409)
		}
		return result, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	_, err = s.db.Exec(`INSERT INTO tool_calls VALUES(?,?,?,'running',NULL,?)`, taskID, opID, argsHash, time.Now().UnixMilli())
	return nil, err
}
func (s *Store) toolFinish(taskID, opID string, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	r, err := s.db.Exec(`UPDATE tool_calls SET state='done',result=?,updated=? WHERE task_id=? AND operation_id=?`, b, time.Now().UnixMilli(), taskID, opID)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return fmt.Errorf("tool journal entry missing")
	}
	return nil
}
