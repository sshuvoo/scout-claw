package filesystem

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"scoutclaw/internal/tools"
)

// State is shared by the filesystem tools of one session. It remembers what
// the agent has read (so edits can be refused when the agent never saw the
// file, or the file changed since) and journals every change so it can be
// listed and undone.
type State struct {
	mu      sync.Mutex
	reads   map[string]string // abs path -> sha256 of the content last seen by the agent
	changes []Change
	journal string // jsonl file; empty keeps the journal in memory only
}

// Change is one journal entry. Path is the target (the source for copy and move).
type Change struct {
	Seq         int       `json:"seq"`
	Time        time.Time `json:"time"`
	Op          string    `json:"op"` // write, patch, lines, mkdir, copy, move, delete, undo
	Path        string    `json:"path"`
	Dest        string    `json:"dest,omitempty"`
	Backup      string    `json:"backup,omitempty"`       // previous file contents
	Trash       string    `json:"trash,omitempty"`        // where a deleted or replaced item was moved
	Created     bool      `json:"created,omitempty"`      // the target did not exist before
	CreatedDirs string    `json:"created_dirs,omitempty"` // top-most directory created by mkdir
	Before      string    `json:"before,omitempty"`       // sha256 before
	After       string    `json:"after,omitempty"`        // sha256 after
	Ref         int       `json:"ref,omitempty"`          // for undo: the entry it reverted
	Undone      bool      `json:"-"`
}

// NewState creates the state; journal is the session's jsonl journal file
// (loaded if it exists, so a rebuilt runtime can still list and undo changes).
func NewState(journal string) *State {
	s := &State{reads: map[string]string{}, journal: journal}
	if journal == "" {
		return s
	}
	f, err := os.Open(journal)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var c Change
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			s.changes = append(s.changes, c)
		}
	}
	s.applyUndoMarks()
	return s
}

func (s *State) applyUndoMarks() {
	for _, c := range s.changes {
		if c.Op == "undo" {
			for i := range s.changes {
				if s.changes[i].Seq == c.Ref {
					s.changes[i].Undone = true
				}
			}
		}
	}
}

func hashOf(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// sawContent records that the agent has seen (or itself produced) this content.
func (s *State) sawContent(abs, raw string) {
	s.mu.Lock()
	s.reads[abs] = hashOf(raw)
	s.mu.Unlock()
}

func (s *State) forget(abs string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := abs + string(filepath.Separator)
	for p := range s.reads {
		if p == abs || strings.HasPrefix(p, prefix) {
			delete(s.reads, p)
		}
	}
}

// moved carries read records from src to dest (a file or a directory tree).
func (s *State) moved(src, dest string) { s.carry(src, dest, false) }

// copied gives dest the read records of src: the content is identical.
func (s *State) copied(src, dest string) { s.carry(src, dest, true) }

func (s *State) carry(src, dest string, keep bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := src + string(filepath.Separator)
	moved := map[string]string{}
	for p, h := range s.reads {
		switch {
		case p == src:
			moved[dest] = h
		case strings.HasPrefix(p, prefix):
			moved[filepath.Join(dest, strings.TrimPrefix(p, prefix))] = h
		default:
			continue
		}
		if !keep {
			delete(s.reads, p)
		}
	}
	for p, h := range moved {
		s.reads[p] = h
	}
}

// checkFresh refuses an edit of an existing file the agent has not read, or
// that changed on disk since the agent last saw it.
func (s *State) checkFresh(abs, raw string) error {
	s.mu.Lock()
	seen, ok := s.reads[abs]
	s.mu.Unlock()
	switch {
	case !ok:
		return tools.Errorf(tools.KindInvalidInput, "%s has not been read in this session; read it with fs_read before editing it", abs)
	case seen != hashOf(raw):
		return tools.Errorf(tools.KindInvalidInput, "%s changed on disk since you last read it; read it again with fs_read before editing", abs)
	}
	return nil
}

// record appends a journal entry and returns it with its sequence number.
func (s *State) record(c Change) Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Seq = 1
	if n := len(s.changes); n > 0 {
		c.Seq = s.changes[n-1].Seq + 1
	}
	c.Time = time.Now().UTC()
	s.changes = append(s.changes, c)
	if c.Op == "undo" {
		s.applyUndoMarks()
	}
	if s.journal != "" {
		if err := os.MkdirAll(filepath.Dir(s.journal), 0o700); err == nil {
			if f, err := os.OpenFile(s.journal, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				b, _ := json.Marshal(c)
				f.Write(append(b, '\n'))
				f.Close()
			}
		}
	}
	return c
}

// Changes returns the journal entries that are still in effect (not undone, not undo markers).
func (s *State) Changes() []Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Change
	for _, c := range s.changes {
		if c.Op != "undo" && !c.Undone {
			out = append(out, c)
		}
	}
	return out
}

// touches reports whether the change involves abs (or something below it).
func (c Change) touches(abs string) bool {
	in := func(p string) bool {
		return p != "" && (p == abs || strings.HasPrefix(p, abs+string(filepath.Separator)))
	}
	return in(c.Path) || in(c.Dest)
}
