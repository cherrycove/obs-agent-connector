package state

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimTracksSignalsAndCompletesOnlyAfterBoth(t *testing.T) {
	manager := Manager{Root: filepath.Join(t.TempDir(), "state")}
	if completed, err := manager.Completed("session", "turn"); err != nil || completed {
		t.Fatalf("new turn completion state = %t, %v", completed, err)
	}
	claim, err := manager.Claim("session", "turn", "fingerprint")
	if err != nil || claim == nil {
		t.Fatalf("claim failed: %v", err)
	}
	if err := claim.MarkSignalUploaded("traces", map[string]any{"status": 200}); err != nil {
		t.Fatal(err)
	}
	if err := claim.Complete("traces", "metrics"); err == nil {
		t.Fatal("completion must fail while metrics is missing")
	}
	if err := claim.MarkSignalUploaded("metrics", map[string]any{"status": 200}); err != nil {
		t.Fatal(err)
	}
	if err := claim.Complete("traces", "metrics"); err != nil {
		t.Fatal(err)
	}
	if completed, err := manager.Completed("session", "turn"); err != nil || !completed {
		t.Fatalf("completed turn state = %t, %v", completed, err)
	}
	if _, err := manager.Claim("session", "turn", "fingerprint"); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("expected completed error, got %v", err)
	}
}

func TestClaimIsExclusiveAndStaleClaimRecovers(t *testing.T) {
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	manager := Manager{
		Root:       filepath.Join(t.TempDir(), "state"),
		StaleAfter: time.Minute,
		Now:        func() time.Time { return now },
	}
	first, err := manager.Claim("session", "turn", "")
	if err != nil || first == nil {
		t.Fatalf("first claim failed: %v", err)
	}
	second, err := manager.Claim("session", "turn", "")
	if err != nil || second != nil {
		t.Fatalf("concurrent claim should be skipped: %v", err)
	}
	now = now.Add(2 * time.Minute)
	recovered, err := manager.Claim("session", "turn", "")
	if err != nil || recovered == nil {
		t.Fatalf("stale claim was not recovered: %v", err)
	}
}
