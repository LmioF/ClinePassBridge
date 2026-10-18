package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRequestLogFailureBoundsMemoryAndRecoversRetainedRows(t *testing.T) {
	dir := t.TempDir()
	s := requestJournalService(t, dir)
	s.cfg.LogRetention = 50
	journalPath := filepath.Join(dir, requestJournalName)
	if err := os.Mkdir(journalPath, 0700); err != nil {
		t.Fatal(err)
	}
	s.stopped = false
	t.Cleanup(func() {
		s.mu.Lock()
		s.stopped = true
		if s.observationTimer != nil {
			s.observationTimer.Stop()
		}
		s.mu.Unlock()
	})
	s.appendLog(LogEntry{ID: "first-failure", Status: 200})
	s.mu.Lock()
	if !s.logStore.needsSnapshot || s.logWriteError == "" {
		s.mu.Unlock()
		t.Fatal("storage failure did not enter snapshot recovery")
	}
	// Hold retries while ordinary traffic continues; the timer must also honor it.
	s.logStore.retryAt = time.Now().Add(time.Hour)
	s.mu.Unlock()
	for i := range 1000 {
		s.appendLog(LogEntry{ID: fmt.Sprintf("during-failure-%d", i), Status: 200})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.logs) != 50 || len(s.logStore.pending) != 0 || s.logStore.sequence != 1001 {
		t.Fatalf("unbounded or lost recovery state: rows=%d pending=%d sequence=%d", len(s.logs), len(s.logStore.pending), s.logStore.sequence)
	}
	if _, err := os.Stat(filepath.Join(dir, requestSnapshotName)); !os.IsNotExist(err) {
		t.Fatalf("traffic bypassed the retry delay: %v", err)
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	s.logStore.retryAt = time.Time{}
	s.flushObservationsLocked()
	if s.logWriteError != "" || s.logStore.needsSnapshot {
		t.Fatalf("recovery failed: %s", s.logWriteError)
	}
	if got := observabilityPersistedLogs(t, dir); !reflect.DeepEqual(got, s.logs) || got[0].ID != "during-failure-950" {
		t.Fatal("recovery resurrected evicted rows or lost retained history")
	}
}

func TestRequestLogRecoveryRetriesAfterSnapshotWasRenamed(t *testing.T) {
	dir := t.TempDir()
	s := requestJournalService(t, dir)
	journalPath := filepath.Join(dir, requestJournalName)
	if err := os.Mkdir(journalPath, 0700); err != nil {
		t.Fatal(err)
	}
	appendBackoff := func(id string) {
		s.appendLog(LogEntry{ID: id, CredentialID: "fixture", Model: "alias", UpstreamModel: "upstream", Status: 429, UpstreamSkipped: true, BackoffWindowID: "window"})
	}
	appendBackoff("first")
	// The snapshot is installed, but the journal reset still fails over a directory.
	appendBackoff("second")
	if _, err := os.Stat(filepath.Join(dir, requestSnapshotName)); err != nil {
		t.Fatal(err)
	}
	if s.logWriteError == "" || !s.logStore.needsSnapshot || len(s.logStore.pending) != 0 {
		t.Fatal("partial checkpoint did not retain bounded recovery state")
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	appendBackoff("third")
	appendBackoff("fourth")
	if got := observabilityPersistedLogs(t, dir); !reflect.DeepEqual(got, s.logs) || len(got) != 1 || got[0].RequestCount != 4 {
		t.Fatalf("partial checkpoint retry lost or duplicated counts: %+v", got)
	}
}
