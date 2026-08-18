package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// queueStore persists only tasks which have not started. Running progress and
// completed history intentionally remain in memory.
type queueStore struct {
	db *sql.DB
}

func queueDatabasePath() string {
	if path := os.Getenv("AMDL_QUEUE_DB"); path != "" {
		return path
	}
	return filepath.Join("data", "queue.db")
}

func openQueueStore(path string) (*queueStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create queue database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open queue database: %w", err)
	}
	db.SetMaxOpenConns(1)

	statements := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		`CREATE TABLE IF NOT EXISTS pending_tasks (
            id TEXT PRIMARY KEY,
            request_json BLOB NOT NULL,
            task_json BLOB,
            created_at INTEGER NOT NULL
        )`,
		"CREATE INDEX IF NOT EXISTS idx_pending_tasks_created_at ON pending_tasks(created_at)",
		"PRAGMA optimize",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize queue database: %w", err)
		}
	}
	if err := ensureQueueColumn(db, "task_json", "BLOB"); err != nil {
		db.Close()
		return nil, err
	}
	return &queueStore{db: db}, nil
}

func ensureQueueColumn(db *sql.DB, name, definition string) error {
	rows, err := db.Query("PRAGMA table_info(pending_tasks)")
	if err != nil {
		return fmt.Errorf("inspect queue database: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("inspect queue column: %w", err)
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect queue columns: %w", err)
	}
	if found {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE pending_tasks ADD COLUMN " + name + " " + definition); err != nil {
		return fmt.Errorf("migrate queue database: %w", err)
	}
	return nil
}

func (s *queueStore) enqueueMany(tasks []*WebTask) error {
	transaction, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("start queue transaction: %w", err)
	}
	defer transaction.Rollback()
	statement, err := transaction.Prepare("INSERT INTO pending_tasks(id, request_json, task_json, created_at) VALUES(?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("prepare queued tasks: %w", err)
	}
	defer statement.Close()
	for _, task := range tasks {
		requestJSON, err := json.Marshal(task.Request)
		if err != nil {
			return fmt.Errorf("encode queued task request: %w", err)
		}
		taskJSON, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("encode queued task: %w", err)
		}
		if _, err := statement.Exec(task.ID, requestJSON, taskJSON, task.CreatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("persist queued task: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit queued tasks: %w", err)
	}
	return nil
}

func (s *queueStore) remove(id string) error {
	if _, err := s.db.Exec("DELETE FROM pending_tasks WHERE id = ?", id); err != nil {
		return fmt.Errorf("remove queued task: %w", err)
	}
	return nil
}

func (s *queueStore) load() ([]*WebTask, error) {
	rows, err := s.db.Query("SELECT id, request_json, task_json, created_at FROM pending_tasks ORDER BY created_at, id")
	if err != nil {
		return nil, fmt.Errorf("load queued tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*WebTask
	for rows.Next() {
		var (
			task        WebTask
			requestJSON []byte
			taskJSON    []byte
			createdAt   int64
		)
		if err := rows.Scan(&task.ID, &requestJSON, &taskJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scan queued task: %w", err)
		}
		if len(taskJSON) > 0 {
			if err := json.Unmarshal(taskJSON, &task); err != nil {
				return nil, fmt.Errorf("decode queued task %s: %w", task.ID, err)
			}
		} else if err := json.Unmarshal(requestJSON, &task.Request); err != nil {
			return nil, fmt.Errorf("decode legacy queued task %s: %w", task.ID, err)
		}
		task.Status = "queued"
		task.CreatedAt = time.UnixMilli(createdAt).UTC()
		tasks = append(tasks, &task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queued tasks: %w", err)
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].BatchID != "" && tasks[i].BatchID == tasks[j].BatchID {
			return tasks[i].QueueIndex < tasks[j].QueueIndex
		}
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	return tasks, nil
}
