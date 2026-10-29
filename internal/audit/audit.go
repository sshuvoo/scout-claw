// Package audit writes an append-only JSON-lines record of tasks, tool
// requests, policy decisions, approvals and results. Everything is redacted
// before it touches disk.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"scoutclaw/internal/redact"
)

type Event struct {
	Time      time.Time      `json:"time"`
	SessionID string         `json:"session_id,omitempty"`
	Kind      string         `json:"kind"` // task_start, tool_request, decision, approval, tool_result, task_end
	Tool      string         `json:"tool,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// Logger is safe for concurrent use. A nil *Logger discards events.
type Logger struct {
	mu  sync.Mutex
	f   *os.File
	red *redact.Redactor
}

func Open(path string, red *redact.Redactor) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f, red: red}, nil
}

func (l *Logger) Log(e Event) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	line := l.red.Redact(string(b))
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.WriteString(line + "\n")
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}

// Read returns events for a session (all sessions if id is empty), oldest first.
func Read(path, sessionID string) ([]Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Event
	start := 0
	for i := 0; i <= len(b); i++ {
		if i == len(b) || b[i] == '\n' {
			if i > start {
				var e Event
				if json.Unmarshal(b[start:i], &e) == nil && (sessionID == "" || e.SessionID == sessionID) {
					out = append(out, e)
				}
			}
			start = i + 1
		}
	}
	return out, nil
}
