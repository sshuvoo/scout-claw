// Package verify decides whether a claimed outcome is backed by evidence.
//
// The rule is deliberately simple and enforced in code: if the agent changed
// something, success may only be reported as "verified" when a command marked
// purpose=verify succeeded *after* the last change. Otherwise the claim is
// downgraded, whatever the model says.
package verify

import (
	"fmt"
	"sync"
)

type Status string

const (
	Verified   Status = "verified"
	Unverified Status = "unverified"
	Failed     Status = "failed"
	NotNeeded  Status = "not_needed" // nothing was changed; the answer is observational
)

func Parse(s string) (Status, bool) {
	switch Status(s) {
	case Verified, Unverified, Failed, NotNeeded:
		return Status(s), true
	}
	return "", false
}

type Check struct {
	Seq      int
	Summary  string
	ExitCode int
	OK       bool
}

// Tracker records changes and verification attempts in order.
type Tracker struct {
	mu        sync.Mutex
	seq       int
	lastMut   int
	mutations []string
	checks    []Check
}

// Reset clears all evidence (start of a new task).
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq, t.lastMut, t.mutations, t.checks = 0, 0, nil, nil
}

// RecordChange notes that state was modified.
func (t *Tracker) RecordChange(summary string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	t.lastMut = t.seq
	t.mutations = append(t.mutations, summary)
}

// RecordCheck notes a verification attempt.
func (t *Tracker) RecordCheck(summary string, exit int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	t.checks = append(t.checks, Check{Seq: t.seq, Summary: summary, ExitCode: exit, OK: ok})
}

func (t *Tracker) Changes() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.mutations...)
}

// Assessment is the evidence-based verdict.
type Assessment struct {
	Status   Status
	Evidence string
	// Rejected is true when the claim should be sent back to the model once
	// (it claimed verification the evidence does not support).
	Rejected bool
	Note     string
}

// Assess compares a claim with the recorded evidence.
func (t *Tracker) Assess(claimed Status, firstAttempt bool) Assessment {
	t.mu.Lock()
	defer t.mu.Unlock()

	var after []Check
	for _, c := range t.checks {
		if c.Seq > t.lastMut {
			after = append(after, c)
		}
	}
	changed := len(t.mutations) > 0
	var lastOK, lastFail *Check
	for i := range after {
		if after[i].OK {
			lastOK = &after[i]
		} else {
			lastFail = &after[i]
		}
	}

	switch {
	case claimed == Failed:
		return Assessment{Status: Failed, Evidence: describe(lastFail, "no failing check recorded")}
	case !changed && claimed == NotNeeded:
		return Assessment{Status: NotNeeded}
	case lastOK != nil && (lastFail == nil || lastFail.Seq < lastOK.Seq):
		return Assessment{Status: Verified, Evidence: fmt.Sprintf("%s (exit %d)", lastOK.Summary, lastOK.ExitCode)}
	case lastFail != nil && claimed == Verified:
		return Assessment{Status: Failed, Evidence: describe(lastFail, ""), Rejected: firstAttempt,
			Note: "the most recent verification command FAILED, so the outcome cannot be reported as verified"}
	case !changed && claimed == Verified:
		// Observational answer backed by prior inspection commands; accept as not needing verification.
		return Assessment{Status: NotNeeded, Note: "no changes were made, so nothing needed verification"}
	case changed && claimed == Verified:
		return Assessment{Status: Unverified, Rejected: firstAttempt,
			Note: "you changed the system but no command with purpose=verify has succeeded since the last change; run a real check (build, test, status, health probe) or report status=unverified"}
	case changed:
		return Assessment{Status: Unverified, Note: "changes were made but no successful verification followed them"}
	}
	return Assessment{Status: NotNeeded}
}

func describe(c *Check, fallback string) string {
	if c == nil {
		return fallback
	}
	return fmt.Sprintf("%s (exit %d)", c.Summary, c.ExitCode)
}
