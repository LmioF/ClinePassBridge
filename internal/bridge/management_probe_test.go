package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestManagementProbeExplicitCredentialEligibility(t *testing.T) {
	for _, path := range []string{"test", "providers"} {
		for _, tc := range []struct {
			name     string
			disabled bool
			paused   bool
		}{
			{"manual-disabled", true, false},
			{"quota-paused", false, true},
			{"manual-disabled-and-quota-paused", true, true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				s := probeService(t)
				t.Cleanup(func() { observabilityShutdown(t, s) })
				c := s.creds["probe-account"]
				c.Disabled = tc.disabled
				if tc.paused {
					c.QuotaBlock = &QuotaBlock{KeyHash: quotaKeyHash(c), Limits: []QuotaBlockLimit{{Type: "weekly"}}}
				}
				stored := quotaTestAuth(t, s, c)
				before := jsonBytes(s.creds[c.ID])
				routing := sseFrame(map[string]any{"routing": map[string]any{"fallbacksAvailable": []string{"deepseek"}, "resolvedProvider": "deepseek"}})
				h := newFakeHost(ssePlan(routing, simpleSSE()))
				s.SetHost(h.call)
				result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
					Method: "POST", Path: apiBase + "/models/" + path, HostCallbackID: "probe-callback",
					Body: jsonBytes(map[string]any{"model": "deepseek-flash", "upstream_id": "cline-pass/deepseek-v4.1-flash", "credential_id": c.ID}),
				}))
				if err != nil {
					t.Fatal(err)
				}
				response := result.(ManagementResponse)
				var body struct {
					OK           bool   `json:"ok"`
					Status       int    `json:"status"`
					CredentialID string `json:"credential_id"`
				}
				if err := json.Unmarshal(response.Body, &body); err != nil {
					t.Fatal(err)
				}
				if tc.paused {
					status := response.StatusCode
					if status == 200 {
						status = body.Status
					}
					if body.OK || status != 400 || len(h.opened) != 0 {
						t.Fatalf("quota-paused probe admitted: response=%s, upstream calls=%d", response.Body, len(h.opened))
					}
				} else if response.StatusCode != 200 || !body.OK || body.Status != 200 || body.CredentialID != c.ID || len(h.opened) != 1 {
					t.Fatalf("explicit disabled probe failed: response=%s, upstream calls=%d", response.Body, len(h.opened))
				}

				// Normal traffic must still reject the same live credential, even
				// with stale enabled storage and management egress metadata.
				stale := c
				stale.Disabled, stale.QuotaBlock = false, nil
				opened := len(h.opened)
				wantStatus := 401
				if !tc.disabled {
					wantStatus = 429
				}
				for _, method := range []string{"executor.execute", "executor.execute_stream"} {
					_, err := s.Handle(method, jsonBytes(ExecutorRequest{
						AuthID: c.ID, Model: "deepseek-flash", StreamID: "client",
						StorageJSON: jsonBytes(stale), Payload: []byte(`{"messages":[{"role":"user","content":"ok"}]}`),
						Metadata: map[string]any{"egress_management": true},
					}))
					if statusOf(err) != wantStatus || len(h.opened) != opened {
						t.Fatalf("%s admitted credential: error=%v, upstream calls=%d", method, err, len(h.opened))
					}
				}
				if !bytes.Equal(before, jsonBytes(s.creds[c.ID])) || !bytes.Equal(stored, quotaTestRead(t, s, c.ID)) {
					t.Fatal("probe or executor changed credential state or persisted manual disablement")
				}
			})
		}
	}
}

func TestManagementProbeAutomaticCredentialEligibility(t *testing.T) {
	s := probeService(t)
	t.Cleanup(func() { observabilityShutdown(t, s) })
	disabled := s.creds["probe-account"]
	disabled.Disabled = true
	s.creds[disabled.ID] = disabled
	paused := Credential{ID: "quota-paused", APIKey: "paused-key"}
	paused.QuotaBlock = &QuotaBlock{KeyHash: quotaKeyHash(paused), Limits: []QuotaBlockLimit{{Type: "weekly"}}}
	s.creds[paused.ID] = paused
	h := newFakeHost(ssePlan(simpleSSE()))
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.http.do_stream" && payload.(map[string]any)["headers"].(http.Header).Get("Authorization") != "Bearer eligible-key" {
			t.Error("automatic probe selected a disabled or quota-paused credential")
		}
		return h.call(method, payload, out)
	})
	for _, eligible := range []bool{false, true} {
		if eligible {
			s.creds["eligible"] = Credential{ID: "eligible", APIKey: "eligible-key"}
		}
		result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
			Method: "POST", Path: apiBase + "/models/providers", HostCallbackID: "probe-callback",
			Body: jsonBytes(map[string]any{"model": "deepseek-flash"}),
		}))
		if err != nil {
			t.Fatal(err)
		}
		response := result.(ManagementResponse)
		if !eligible && (response.StatusCode != 400 || len(h.opened) != 0) {
			t.Fatalf("automatic probe admitted ineligible credentials: %s", response.Body)
		}
		if eligible && (response.StatusCode != 200 || len(h.opened) != 1) {
			t.Fatalf("automatic probe skipped eligible credential: %s", response.Body)
		}
	}
}
