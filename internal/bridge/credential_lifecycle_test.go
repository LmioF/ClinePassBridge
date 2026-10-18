package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func deletionFixture(t *testing.T) (*Service, Credential, string, []byte) {
	t.Helper()
	s := NewService()
	root := t.TempDir()
	s.cfg.DataDir = filepath.Join(root, "state")
	s.authDir = filepath.Join(root, "auth")
	c := Credential{Type: Provider, ID: "deletion-fixture", Label: "Fixture", APIKey: "fixture-key-original"}
	path := filepath.Join(s.authDir, "fixture.json")
	if err := atomicJSON(path, c); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(s.cfg.DataDir, errorRulesFile), errorRulesDocument{Rules: s.currentErrorRules()}); err != nil {
		t.Fatal(err)
	}
	request := jsonBytes(map[string]any{
		"FileName": filepath.Base(path), "RawJSON": jsonBytes(c),
		"Host": map[string]string{"AuthDir": s.authDir},
	})
	if _, err := s.parseAuth(request); err != nil {
		t.Fatal(err)
	}
	return s, c, path, request
}

func TestDeleteCredentialWaitsForWatcherFileWrites(t *testing.T) {
	s, c, path, _ := deletionFixture(t)
	s.authFileMu.Lock()
	fileLocked := true
	defer func() {
		if fileLocked {
			s.authFileMu.Unlock()
		}
	}()
	deleted := make(chan int, 1)
	go func() {
		result, err := s.deleteCredential(c.ID)
		if err != nil {
			deleted <- 0
			return
		}
		deleted <- result.(ManagementResponse).StatusCode
	}()
	select {
	case status := <-deleted:
		t.Fatalf("deletion bypassed active watcher write: status=%d", status)
	case <-time.After(50 * time.Millisecond):
	}
	// Complete the pending watcher replacement before deletion takes its turn.
	if err := atomicJSON(path, s.creds[c.ID]); err != nil {
		t.Fatal(err)
	}
	s.authFileMu.Unlock()
	fileLocked = false
	select {
	case status := <-deleted:
		if status != 200 {
			t.Fatalf("delete status=%d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deletion did not finish after the watcher released its file lock")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("watcher replacement survived deletion: %v", err)
	}
}

func TestDeletedCredentialIgnoresLateParseAndAllowsPhysicalReimport(t *testing.T) {
	for _, replaceKey := range []bool{false, true} {
		name := "same-key"
		if replaceKey {
			name = "replacement-key"
		}
		t.Run(name, func(t *testing.T) {
			s, c, path, request := deletionFixture(t)
			result, err := s.deleteCredential(c.ID)
			if err != nil || result.(ManagementResponse).StatusCode != 200 {
				t.Fatalf("delete failed: %v", err)
			}
			parsed, err := s.parseAuth(request)
			if err != nil {
				t.Fatal(err)
			}
			response := parsed.(map[string]any)
			if response["Handled"] != false || response["Auth"] != nil {
				t.Fatal("late RawJSON callback registered a deleted credential")
			}
			if _, exists := s.creds[c.ID]; exists {
				t.Fatal("late callback restored the in-memory credential")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("late callback recreated the credential file: %v", err)
			}
			if replaceKey {
				c.APIKey = "fixture-key-replacement"
			}
			if err := atomicJSON(path, c); err != nil {
				t.Fatal(err)
			}
			// Even an old callback snapshot must resolve the freshly imported file.
			parsed, err = s.parseAuth(request)
			if err != nil || parsed.(map[string]any)["Handled"] != true {
				t.Fatalf("physical reimport rejected: %v", err)
			}
			selected, err := s.selectedCredential(ExecutorRequest{AuthID: c.ID})
			if err != nil || selected.APIKey != c.APIKey || s.revoked[c.ID] {
				t.Fatal("physical reimport did not restore the current credential")
			}
		})
	}
}
