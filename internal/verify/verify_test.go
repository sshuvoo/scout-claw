package verify

import "testing"

func TestObservationOnlyNeedsNoVerification(t *testing.T) {
	var tr Tracker
	if a := tr.Assess(NotNeeded, true); a.Status != NotNeeded || a.Rejected {
		t.Fatalf("%+v", a)
	}
	if a := tr.Assess(Verified, true); a.Status != NotNeeded || a.Rejected {
		t.Fatalf("verified claim with no changes is simply observational: %+v", a)
	}
}

func TestChangeWithoutVerificationIsRejectedOnceThenDowngraded(t *testing.T) {
	var tr Tracker
	tr.RecordChange("edit config")
	a := tr.Assess(Verified, true)
	if !a.Rejected || a.Status != Unverified {
		t.Fatalf("%+v", a)
	}
	a = tr.Assess(Verified, false)
	if a.Rejected || a.Status != Unverified || a.Note == "" {
		t.Fatalf("second attempt must be downgraded, not rejected: %+v", a)
	}
	if a := tr.Assess(Unverified, true); a.Rejected || a.Status != Unverified {
		t.Fatalf("honest unverified is accepted: %+v", a)
	}
}

func TestVerificationBeforeChangeDoesNotCount(t *testing.T) {
	var tr Tracker
	tr.RecordCheck("go test", 0, true)
	tr.RecordChange("edit")
	if a := tr.Assess(Verified, false); a.Status != Unverified {
		t.Fatalf("stale check counted: %+v", a)
	}
	tr.RecordCheck("go test", 0, true)
	if a := tr.Assess(Verified, true); a.Status != Verified || a.Evidence == "" {
		t.Fatalf("%+v", a)
	}
}

func TestFailedCheckPreventsVerified(t *testing.T) {
	var tr Tracker
	tr.RecordChange("edit")
	tr.RecordCheck("go test", 1, false)
	if a := tr.Assess(Verified, true); a.Status != Failed || !a.Rejected {
		t.Fatalf("%+v", a)
	}
	tr.RecordCheck("go test", 0, true)
	if a := tr.Assess(Verified, true); a.Status != Verified {
		t.Fatalf("later success supersedes earlier failure: %+v", a)
	}
}

func TestResetClearsEvidence(t *testing.T) {
	var tr Tracker
	tr.RecordChange("x")
	tr.Reset()
	if len(tr.Changes()) != 0 {
		t.Fatal("reset failed")
	}
}
