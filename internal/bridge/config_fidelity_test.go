package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestConfigFidelityRejectedPUTPreservesStateAndDisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "duplicate-providers",
			body: `{"models":[{"id":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","providers":["novita","NOVITA"]}]}`,
		},
		{
			name: "invalid-field-type-after-providers",
			body: `{"models":[{"id":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","providers":["novita","wafer"]}],"log_retention":"invalid"}`,
		},
		{
			name: "malformed-json",
			body: `{"models":[{"id":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","providers":["novita","wafer"]}],`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			s.cfg.DataDir = t.TempDir()
			s.cfg.Models = []Model{{
				ID: "deepseek-flash", UpstreamID: "cline-pass/deepseek-v4.1-flash",
				Providers: []string{"deepseek", "wafer"},
			}}
			path := filepath.Join(s.cfg.DataDir, "settings.json")
			if err := atomicJSON(path, s.cfg); err != nil {
				t.Fatal(err)
			}
			beforeDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Freeze the snapshot as bytes: another config() result could share
			// the same provider backing array and hide a rejected write.
			beforeConfig := jsonBytes(s.config())
			result, err := s.management(jsonBytes(ManagementRequest{
				Method: "PUT", Path: apiBase + "/config", Body: []byte(tc.body),
			}))
			if err != nil {
				t.Fatal(err)
			}
			response, ok := result.(ManagementResponse)
			if !ok {
				t.Fatalf("unexpected management response type %T", result)
			}
			if response.StatusCode != 400 {
				t.Errorf("status = %d, want 400", response.StatusCode)
			}
			if !bytes.Equal(beforeConfig, jsonBytes(s.config())) {
				t.Error("rejected config PUT changed the in-memory configuration")
			}
			afterDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeDisk, afterDisk) {
				t.Error("rejected config PUT changed settings.json")
			}
		})
	}
}

func TestConfigFidelityCopyPreservesProviderShapeAndIsolation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []string
	}{
		{name: "nil", providers: nil},
		{name: "explicit-empty", providers: []string{}},
		{name: "populated", providers: []string{"deepseek", "wafer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			s.cfg.Models = []Model{{ID: "model", UpstreamID: "cline-pass/model", Providers: tc.providers}}
			before := jsonBytes(s.config())
			copied := s.config()
			if !reflect.DeepEqual(copied.Models[0].Providers, tc.providers) {
				t.Error("config copy changed nil, empty or populated provider semantics")
			}
			copied.Models[0].ID = "edited-copy"
			if len(copied.Models[0].Providers) > 0 {
				copied.Models[0].Providers[0] = "novita"
			} else {
				copied.Models[0].Providers = append(copied.Models[0].Providers, "novita")
			}
			if !bytes.Equal(before, jsonBytes(s.config())) {
				t.Error("editing a config copy changed the service configuration")
			}
		})
	}
}

func TestConfigFidelityUnrotatedCredentialSavePreservesUnknown(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		clearPriority bool
	}{
		{name: "label-only", body: `{"label":"renamed"}`},
		{name: "clear-priority", body: `{"label":"renamed","priority":0}`, clearPriority: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAffinityFixture(t)
			const credentialID, filename = "stable-0", "migrated-a.json"
			if f.s.creds[credentialID].RoutingID != "" {
				t.Fatal("fixture credential must have an unrotated identity")
			}
			beforeFields := configFidelityCredentialFields(t, f.original[filename])
			saveCalls := 0
			f.s.SetHost(func(method string, payload, out any) error {
				saveCalls++
				return fmt.Errorf("physical credential must use the plugin parser via its file watcher, got %s", method)
			})
			affinityRequest(t, f, ManagementRequest{
				Method: "PUT", Path: apiBase + "/credentials",
				Query: url.Values{"id": {credentialID}}, Body: []byte(tc.body),
			}, 200)
			if saveCalls != 0 {
				t.Fatalf("generic host save bypassed the quota-aware parser: %d calls", saveCalls)
			}
			savedDisk, err := os.ReadFile(filepath.Join(f.s.authDir, filename))
			if err != nil {
				t.Fatal(err)
			}
			changed := []string{"label", "request_scoped_errors"}
			if tc.clearPriority {
				changed = append(changed, "priority")
			}
			for _, saved := range []struct {
				name string
				raw  []byte
			}{
				{name: "saved-file", raw: savedDisk},
			} {
				t.Run(saved.name, func(t *testing.T) {
					fields := configFidelityCredentialFields(t, saved.raw)
					if !bytes.Equal(jsonBytes(beforeFields["unknown"]), jsonBytes(fields["unknown"])) {
						t.Error("unknown extension was dropped or its large integer changed")
					}
					assertAffinityFields(t, f.original[filename], saved.raw, changed...)
					if strValue := fields["label"]; !bytes.Equal(strValue, []byte(`"renamed"`)) {
						t.Error("credential label was not updated")
					}
					if _, present := fields["priority"]; tc.clearPriority && present {
						t.Error("cleared priority remained in the saved credential")
					}
				})
			}
			credential := f.s.creds[credentialID]
			if credential.Label != "renamed" || credential.RoutingID != "" {
				t.Error("credential edit did not retain its label and unrotated identity")
			}
			if tc.clearPriority {
				if credential.Priority != nil {
					t.Error("cleared priority remained in memory")
				}
			} else if priorityValue(credential) != 1 {
				t.Error("label-only edit changed credential priority")
			}
			if !bytes.Equal(f.files()["migrated-b.json"], f.original["migrated-b.json"]) {
				t.Error("editing one credential changed another credential file")
			}
		})
	}
}

func configFidelityCredentialFields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		t.Fatal("credential fixture is not a JSON object")
	}
	return fields
}

func TestConfigFidelityLegacyQuotaAutoPauseIgnored(t *testing.T) {
	for _, settings := range []bool{false, true} {
		t.Run(fmt.Sprintf("settings-%t", settings), func(t *testing.T) {
			s := NewService()
			dataDir := t.TempDir()
			path := filepath.Join(dataDir, "settings.json")
			if settings {
				if err := os.WriteFile(path, []byte(`{"quota_auto_pause":false,"timeout_seconds":120}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			configYAML := fmt.Sprintf("data_dir: %q\nquota_auto_pause: false\n", filepath.ToSlash(dataDir))
			if err := s.configure(jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); err != nil {
				t.Fatal(err)
			}
			if settings && s.config().TimeoutSeconds != 120 {
				t.Fatal("legacy quota option caused unrelated settings to be discarded")
			}
			if bytes.Contains(jsonBytes(s.config()), []byte("quota_auto_pause")) {
				t.Fatal("removed quota option remained in public configuration")
			}
			result, err := s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/config", Body: []byte(`{"quota_auto_pause":false,"timeout_seconds":180}`)}))
			if err != nil || result.(ManagementResponse).StatusCode != 200 {
				t.Fatalf("legacy config update was not compatible: %v", err)
			}
			stored, err := os.ReadFile(path)
			if err != nil || bytes.Contains(stored, []byte("quota_auto_pause")) || s.config().TimeoutSeconds != 180 {
				t.Fatal("legacy config option was persisted or an unrelated update was lost")
			}
			result, err = s.management(jsonBytes(ManagementRequest{Method: "PUT", Path: apiBase + "/scheduling", Body: []byte(`{"quota_auto_pause":false}`)}))
			if err != nil || result.(ManagementResponse).StatusCode != 404 {
				t.Fatalf("removed scheduling write endpoint is still accepted: %v", err)
			}
			c := Credential{ID: "one", APIKey: "quota-key"}
			now := time.Now().UTC()
			c.QuotaBlock = &QuotaBlock{KeyHash: quotaKeyHash(c), Source: "usage", BlockedAt: now, ObservedAt: now, Limits: []QuotaBlockLimit{{Type: "weekly"}}}
			if authData(c, "one.json").(map[string]any)["Disabled"] != true {
				t.Fatal("legacy false configuration disabled quota protection")
			}
			// Inspect actual route registration as well as dispatcher behavior.
			routes, err := s.registerManagement(nil)
			_, _ = s.Handle("plugin.shutdown", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range routes.(map[string]any)["routes"].([]map[string]string) {
				if route["Method"] == "PUT" && route["Path"] == apiBase+"/scheduling" {
					t.Fatal("removed scheduling write endpoint remains registered")
				}
			}
		})
	}
}
