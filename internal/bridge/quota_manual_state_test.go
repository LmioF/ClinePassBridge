package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuotaHostSaveRoundTripPreservesManualState(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		manualDisabled        bool
		legacyMissingSnapshot bool
		toggleDuringPause     bool
	}{
		{"automatic-pause", false, false, false},
		{"manual-disabled", true, false, false},
		{"legacy-disabled-without-snapshot", true, true, false},
		{"disable-during-pause", false, false, true},
		{"enable-during-pause", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			c := Credential{ID: "round-trip", APIKey: "fixture-round-trip-key", Disabled: tc.manualDisabled}
			quotaTestAuth(t, s, c)
			observed := time.Now().UTC()
			exhausted := quotaTestRoom("round-trip-account", observed)
			percent := 100.0
			exhausted.Limits[0].PercentUsed = &percent
			if err := s.observeQuotaUsage(s.creds[c.ID], exhausted, observed.Add(-time.Second)); err != nil {
				t.Fatal("could not persist exhausted quota")
			}

			// CPA FileStore.Save projects the effective Auth.Disabled into
			// metadata; pluginTokenStorage merges that metadata into StorageJSON.
			// Preserve every other field, including any separate manual marker.
			simulateHostSave := func(service *Service) {
				t.Helper()
				auth := quotaTestParse(t, service, c.ID, quotaTestRead(t, service, c.ID))
				if auth["Disabled"] != true {
					t.Fatal("quota exhaustion did not disable the host auth")
				}
				var stored, metadata map[string]json.RawMessage
				if err := json.Unmarshal(auth["StorageJSON"].([]byte), &stored); err != nil {
					t.Fatal("invalid auth storage JSON")
				}
				if err := json.Unmarshal(jsonBytes(auth["Metadata"]), &metadata); err != nil {
					t.Fatal("invalid auth metadata")
				}
				metadata["disabled"] = jsonBytes(auth["Disabled"])
				for key, value := range metadata {
					stored[key] = value
				}
				if tc.legacyMissingSnapshot {
					// A legacy file has only disabled=true plus a quota block.
					// Its origin is unknown, so recovery must retain manual disable.
					var block map[string]json.RawMessage
					if err := json.Unmarshal(stored["quota_block"], &block); err != nil || block == nil {
						t.Fatal("missing legacy quota block fixture")
					}
					delete(block, "manual_disabled")
					stored["quota_block"] = jsonBytes(block)
				}
				if err := os.WriteFile(filepath.Join(service.authDir, c.ID+".json"), jsonBytes(stored), 0600); err != nil {
					t.Fatal("could not persist simulated host save")
				}
			}
			simulateHostSave(s)

			assertState := func(service *Service, stage string, paused bool) {
				t.Helper()
				items := service.credentials()
				if len(items) != 1 {
					t.Fatalf("%s: expected one credential", stage)
				}
				item := items[0]
				if item["manual_disabled"] != tc.manualDisabled || item["enabled"] != !tc.manualDisabled || item["quota_paused"] != paused || item["effective_disabled"] != (tc.manualDisabled || paused) {
					t.Errorf("%s: manual_disabled=%v enabled=%v quota_paused=%v effective_disabled=%v; want %v %v %v %v",
						stage, item["manual_disabled"], item["enabled"], item["quota_paused"], item["effective_disabled"],
						tc.manualDisabled, !tc.manualDisabled, paused, tc.manualDisabled || paused)
				}
			}
			quotaTestParse(t, s, c.ID, quotaTestRead(t, s, c.ID))
			assertState(s, "watcher reparse after host save", true)
			restarted := NewService()
			restarted.authDir = s.authDir
			quotaTestParse(t, restarted, c.ID, quotaTestRead(t, s, c.ID))
			assertState(restarted, "restart after host save", true)
			if tc.toggleDuringPause {
				result, err := restarted.management(jsonBytes(ManagementRequest{
					Method: "PUT", Path: apiBase + "/scheduling/credential",
					Body: jsonBytes(map[string]any{"id": c.ID, "enabled": tc.manualDisabled}),
				}))
				if err != nil || result.(ManagementResponse).StatusCode != 200 {
					t.Fatal("manual toggle during quota pause failed")
				}
				tc.manualDisabled = !tc.manualDisabled
				assertState(restarted, "manual toggle during quota pause", true)
				simulateHostSave(restarted)
				quotaTestParse(t, restarted, c.ID, quotaTestRead(t, restarted, c.ID))
				assertState(restarted, "host save after manual toggle", true)
				restarted = NewService()
				restarted.authDir = s.authDir
				quotaTestParse(t, restarted, c.ID, quotaTestRead(t, s, c.ID))
				assertState(restarted, "restart after manual toggle and host save", true)
			}

			checked := observed.Add(time.Second)
			if err := restarted.observeQuotaUsage(restarted.creds[c.ID], quotaTestRoom("round-trip-account", checked), checked); err != nil {
				t.Fatal("could not persist fresh quota recovery")
			}
			assertState(restarted, "fresh quota recovery", false)
			_, err := restarted.selectedCredential(ExecutorRequest{AuthID: c.ID})
			if tc.manualDisabled {
				if statusOf(err) != 401 {
					t.Error("quota recovery admitted a manually disabled credential")
				}
			} else if err != nil {
				t.Errorf("quota recovery left an automatically paused credential unschedulable: status=%d", statusOf(err))
			}
			reloaded := NewService()
			reloaded.authDir = s.authDir
			auth := quotaTestParse(t, reloaded, c.ID, quotaTestRead(t, restarted, c.ID))
			assertState(reloaded, "restart after quota recovery", false)
			if auth["Disabled"] != tc.manualDisabled {
				t.Error("recovered host auth does not preserve the original manual state")
			}
		})
	}
}
