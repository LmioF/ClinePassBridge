package bridge

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveCredentialUsesLatestPhysicalExtensions(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "updated-and-added"
		if remove {
			name = "removed"
		}
		t.Run(name, func(t *testing.T) {
			f := newAffinityFixture(t)
			const credentialID, filename = "stable-0", "migrated-a.json"
			path := filepath.Join(f.s.authDir, filename)
			baseline := f.s.creds[credentialID]
			var latest map[string]json.RawMessage
			if err := json.Unmarshal(f.original[filename], &latest); err != nil {
				t.Fatal(err)
			}
			if remove {
				delete(latest, "unknown")
			} else {
				latest["unknown"] = json.RawMessage(`{"serial":9007199254740995,"version":"latest"}`)
				latest["host_added"] = json.RawMessage(`9007199254740997`)
			}
			if err := atomicJSON(path, latest); err != nil {
				t.Fatal(err)
			}
			// The watcher has not parsed the external edit, so the baseline still
			// contains the previous extension values.
			edited := baseline
			edited.Label = "renamed"
			f.s.credentialMu.Lock()
			err := f.s.saveCredential(edited, baseline)
			f.s.credentialMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertAffinityFields(t, jsonBytes(latest), saved, "label", "request_scoped_errors")
			var disk Credential
			if err := json.Unmarshal(saved, &disk); err != nil {
				t.Fatal(err)
			}
			current := f.s.creds[credentialID]
			if !bytes.Equal(jsonBytes(current.extraFields), jsonBytes(disk.extraFields)) {
				t.Fatal("saved in-memory extensions do not match the latest physical file")
			}
			if remove {
				if _, restored := current.extraFields["unknown"]; restored {
					t.Fatal("removed extension was restored")
				}
			} else if string(current.extraFields["host_added"]) != "9007199254740997" {
				t.Fatal("new extension or its exact JSON integer was lost")
			}
		})
	}
}
