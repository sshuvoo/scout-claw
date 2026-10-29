// Package session persists task history so it can be listed and resumed.
// Sessions are plain JSON files (one per session) in the state directory; the
// agent works fine without persistence.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type TaskRecord struct {
	Prompt    string    `json:"prompt"`
	Status    string    `json:"status"` // completed, blocked, cancelled, limit, error
	Verified  string    `json:"verification,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Changes   []string  `json:"changes,omitempty"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Steps     int       `json:"steps"`
	TokensIn  int64     `json:"tokens_in,omitempty"`
	TokensOut int64     `json:"tokens_out,omitempty"`
}

// Pending is a tool output not yet delivered to the provider.
type Pending struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type Session struct {
	ID         string       `json:"id"`
	Created    time.Time    `json:"created"`
	Updated    time.Time    `json:"updated"`
	Workspace  string       `json:"workspace"`
	Model      string       `json:"model"`
	ResponseID string       `json:"response_id,omitempty"`     // provider-side conversation pointer for resuming
	Pending    []Pending    `json:"pending_outputs,omitempty"` // tool outputs the provider still expects
	Tasks      []TaskRecord `json:"tasks"`

	dir string
}

// New creates a session; it is written to disk on the first Save.
func New(dir, workspace, model string) *Session {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	now := time.Now().UTC()
	return &Session{
		ID:      now.Format("20060102-150405") + "-" + hex.EncodeToString(b),
		Created: now, Updated: now, Workspace: workspace, Model: model, dir: dir,
	}
}

func (s *Session) Save() error {
	s.Updated = time.Now().UTC()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, s.ID+".json"))
}

func Load(dir, id string) (*Session, error) {
	if id == "" || id != filepath.Base(id) {
		return nil, errors.New("invalid session id")
	}
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("session %q: %w", id, err)
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("session %q is corrupt: %w", id, err)
	}
	s.dir = dir
	return &s, nil
}

// List returns sessions, newest first. Unreadable files are skipped.
func List(dir string) ([]*Session, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if s, err := Load(dir, e.Name()[:len(e.Name())-5]); err == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Latest returns the most recently updated session, or nil.
func Latest(dir string) (*Session, error) {
	l, err := List(dir)
	if err != nil || len(l) == 0 {
		return nil, err
	}
	return l[0], nil
}
