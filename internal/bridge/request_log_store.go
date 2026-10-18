package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const requestJournalName = "requests.journal.jsonl"
const requestSnapshotName = "requests.snapshot.json"

type requestLogEvent struct {
	Sequence  uint64    `json:"sequence"`
	Entry     *LogEntry `json:"entry,omitempty"`
	Merged    bool      `json:"merged,omitempty"`
	Retention int       `json:"retention"`
	Clear     bool      `json:"clear,omitempty"`
}

type requestLogSnapshot struct {
	Version  int        `json:"version"`
	Sequence uint64     `json:"sequence"`
	Logs     []LogEntry `json:"logs"`
}

type requestLogStore struct {
	sequence      uint64
	journalBytes  int64 // Last complete, successfully written line; trim failed/torn tails here.
	journalEvents int
	snapshotBytes int64
	pending       []requestLogEvent
	needsSnapshot bool // After a write failure, s.logs is the bounded recovery state.
	retryAt       time.Time
}

func (s *Service) loadRequestLogsLocked() error {
	s.logStore = requestLogStore{}
	s.logs = nil
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, requestSnapshotName))
	if err == nil {
		var snapshot requestLogSnapshot
		if err = json.Unmarshal(b, &snapshot); err != nil {
			return fmt.Errorf("read request log snapshot: %w", err)
		}
		if snapshot.Version != 1 {
			return errors.New("unsupported request log snapshot version")
		}
		s.logs, s.logStore.sequence = snapshot.Logs, snapshot.Sequence
		s.logStore.snapshotBytes = int64(len(b))
	} else if errors.Is(err, os.ErrNotExist) {
		b, err = os.ReadFile(filepath.Join(s.cfg.DataDir, "requests.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal(b, &s.logs); err != nil {
				return fmt.Errorf("read legacy request logs: %w", err)
			}
			s.logStore.snapshotBytes = int64(len(b))
		}
		s.logs = compactBackoffLogs(s.logs)
	} else {
		return err
	}
	f, err := os.Open(filepath.Join(s.cfg.DataDir, requestJournalName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var previous uint64
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// A killed writer may leave a partial last line. Never append behind it.
			return nil
		}
		if err != nil {
			return err
		}
		var event requestLogEvent
		if err = json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("request journal at byte %d: %w", s.logStore.journalBytes, err)
		}
		if event.Sequence == 0 || event.Sequence <= previous || event.Retention < 1 || event.Retention > 10000 {
			return errors.New("invalid request journal sequence or retention")
		}
		previous = event.Sequence
		if event.Sequence > s.logStore.sequence {
			if event.Sequence != s.logStore.sequence+1 {
				return errors.New("request journal sequence gap")
			}
			if err = s.applyRequestLogEventLocked(event); err != nil {
				return err
			}
			s.logStore.sequence = event.Sequence
			s.logStore.journalEvents++
		}
		s.logStore.journalBytes += int64(len(line))
	}
}

func (s *Service) applyRequestLogEventLocked(event requestLogEvent) error {
	if event.Clear {
		s.logs = nil
	}
	if event.Entry != nil {
		if event.Merged {
			key := backoffLogKey(*event.Entry)
			found := false
			for i := len(s.logs) - 1; i >= 0; i-- {
				if key != "" && backoffLogKey(s.logs[i]) == key {
					s.logs = append(s.logs[:i], s.logs[i+1:]...)
					found = true
					break
				}
			}
			if !found {
				return errors.New("request journal merge target missing")
			}
		}
		// Already aggregated absolute values: replay must never add counts again.
		s.logs = append(s.logs, *event.Entry)
	}
	if len(s.logs) > event.Retention {
		s.logs = s.logs[len(s.logs)-event.Retention:]
	}
	return nil
}

func (s *Service) queueRequestLogLocked(entry *LogEntry, merged bool) {
	s.logStore.sequence++
	if s.logStore.needsSnapshot {
		return
	}
	event := requestLogEvent{Sequence: s.logStore.sequence, Retention: s.cfg.LogRetention, Merged: merged}
	if entry != nil {
		copy := *entry
		event.Entry = &copy
	}
	s.logStore.pending = append(s.logStore.pending, event)
}

// Every completion writes its own line before returning; no in-memory batching.
// Like the previous requests.json writer, ordinary appends do not call fsync.
func (s *Service) flushRequestLogsLocked() {
	// Failed storage must not turn request traffic into an ever-growing queue
	// or a full snapshot rewrite per request. Shutdown makes one final attempt.
	if s.logStore.needsSnapshot && !s.stopped && time.Now().Before(s.logStore.retryAt) {
		s.scheduleObservationsLocked()
		return
	}
	var err error
	if s.logStore.needsSnapshot {
		err = s.compactRequestLogsLocked()
	} else {
		err = s.writeRequestJournalLocked()
	}
	if err == nil && (s.logStore.journalBytes >= 64<<20 ||
		s.logStore.journalEvents >= 1024 && s.logStore.journalBytes >= s.logStore.snapshotBytes) {
		err = s.compactRequestLogsLocked()
	}
	s.logWriteError = safeError(err)
	if err != nil {
		// The current retained rows include every pending absolute update. A
		// checkpoint can safely replace failed/partial appends once IO recovers.
		s.logStore.pending = nil
		s.logStore.needsSnapshot = true
		s.logStore.retryAt = time.Now().Add(2 * time.Second)
		s.scheduleObservationsLocked()
	} else {
		s.logStore.retryAt = time.Time{}
	}
}

func (s *Service) writeRequestJournalLocked() error {
	if len(s.logStore.pending) == 0 {
		return nil
	}
	var data bytes.Buffer
	for _, event := range s.logStore.pending {
		line, err := json.Marshal(event)
		if err != nil {
			return err
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	f, err := os.OpenFile(filepath.Join(s.cfg.DataDir, requestJournalName), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil && info.Size() < s.logStore.journalBytes {
		err = errors.New("request journal unexpectedly shortened")
	}
	if err == nil && info.Size() > s.logStore.journalBytes {
		err = f.Truncate(s.logStore.journalBytes)
	}
	if err == nil {
		_, err = f.Seek(s.logStore.journalBytes, io.SeekStart)
	}
	if err == nil {
		_, err = f.Write(data.Bytes())
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err // Retry truncates any partial append before replaying pending events.
	}
	s.logStore.journalBytes += int64(data.Len())
	s.logStore.journalEvents += len(s.logStore.pending)
	s.logStore.pending = nil
	return nil
}

func (s *Service) compactRequestLogsLocked() error {
	data, err := json.Marshal(requestLogSnapshot{Version: 1, Sequence: s.logStore.sequence, Logs: s.logs})
	if err != nil {
		return err
	}
	path := filepath.Join(s.cfg.DataDir, requestSnapshotName)
	tmp := path + ".tmp-" + id()
	defer os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// The snapshot and its directory entry must reach disk before deleting WAL.
	// Windows does not implement directory fsync; production shared libraries run on Linux.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(s.cfg.DataDir)
		if err != nil {
			return err
		}
		if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	// The first failed append may never have created a journal at all.
	journal, err := os.OpenFile(filepath.Join(s.cfg.DataDir, requestJournalName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err = journal.Close(); err != nil {
		return err
	}
	s.logStore.journalBytes, s.logStore.journalEvents = 0, 0
	s.logStore.snapshotBytes = int64(len(data))
	s.logStore.pending, s.logStore.needsSnapshot = nil, false
	return nil
}
