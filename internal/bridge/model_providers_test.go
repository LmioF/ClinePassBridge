package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderDiscoveryIgnoresConfiguredPins(t *testing.T) {
	for _, policy := range []string{ProviderPolicyConfig, ProviderPolicyClient} {
		t.Run(policy, func(t *testing.T) {
			s := probeService(t)
			t.Cleanup(func() { observabilityShutdown(t, s) })
			s.cfg.ProviderPolicy = policy
			s.cfg.Models[0].Providers = []string{"deepseek", "wafer"}
			routing := sseFrame(map[string]any{"routing": map[string]any{
				"fallbacksAvailable": []string{"alibaba", "wafer"}, "resolvedProvider": "deepseek",
			}})
			host := &capturingHost{plan: ssePlan(routing, simpleSSE())}
			s.SetHost(host.call)

			result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
				Method: "POST", Path: apiBase + "/models/providers", HostCallbackID: "probe-callback",
				Body: jsonBytes(map[string]any{"model": "deepseek-flash", "credential_id": "probe-account"}),
			}))
			if err != nil {
				t.Fatal(err)
			}
			response := result.(ManagementResponse)
			var body struct {
				OK        bool     `json:"ok"`
				Status    int      `json:"status"`
				Providers []string `json:"providers"`
				ServedBy  string   `json:"served_by"`
			}
			if err := json.Unmarshal(response.Body, &body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || !body.OK || body.Status != 200 || body.ServedBy != "deepseek" {
				t.Fatalf("provider discovery failed: %s", response.Body)
			}
			if got := strings.Join(body.Providers, ","); got != "alibaba,wafer,deepseek" {
				t.Errorf("providers = %q, want candidates beyond the configured pins", got)
			}

			host.mu.Lock()
			defer host.mu.Unlock()
			if len(host.bodies) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(host.bodies))
			}
			probe := host.bodies[0]
			for _, key := range []string{"provider", "providerOptions"} {
				if _, present := probe[key]; present {
					t.Errorf("provider discovery retained %s: %s", key, jsonBytes(probe))
				}
			}
			if probe["model"] != "cline-pass/deepseek-v4.1-flash" || probe["stream"] != true || probe["max_tokens"] != float64(16) {
				t.Errorf("unexpected probe settings: %s", jsonBytes(probe))
			}
			messages := list(probe["messages"])
			if len(messages) != 1 || object(messages[0])["role"] != "user" || object(messages[0])["content"] != "ok" {
				t.Errorf("unexpected probe messages: %s", jsonBytes(probe))
			}
		})
	}
}
