package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulingOldHostAndProxySecretRedaction(t *testing.T) {
	s := registeredService(t, "")
	var callbacks []string
	s.SetHost(func(method string, _, _ any) error {
		callbacks = append(callbacks, method)
		return errors.New("unsupported callback")
	})
	config := `{"enabled":false,"endpoints":[{"id":"p1","enabled":true,"proxy_url":"http://private-user:private-password@localhost:8080","group":"shared","rpm":60,"max_inflight":4}]}`
	if _, err := s.saveEgressConfig([]byte(config)); err != nil {
		t.Fatal(err)
	}
	result, err := s.egressManagementResponse()
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.(ManagementResponse).Body)
	if strings.Contains(body, "private-user") || strings.Contains(body, "private-password") || !strings.Contains(body, `"has_auth":true`) {
		t.Fatalf("bad public config: %s", body)
	}
	result, err = s.schedulingResponse()
	if err != nil || result.(ManagementResponse).StatusCode != 200 {
		t.Fatalf("old host rejected: %v", err)
	}
	body = string(result.(ManagementResponse).Body)
	if !strings.Contains(body, `"available":false`) || strings.Contains(body, `"local_delay_supported"`) || strings.Contains(body, `"request_cancel_observable"`) {
		t.Fatal("old host availability was lost or obsolete capabilities were returned")
	}
	if len(callbacks) != 1 || callbacks[0] != "host.auth.list" {
		t.Fatalf("scheduling performed an obsolete capabilities probe: %v", callbacks)
	}
}

func TestSchedulingQuotaManualStateAndHostProjection(t *testing.T) {
	s := NewService()
	c := Credential{ID: "quota", APIKey: "quota-key"}
	now := time.Now().UTC()
	c.QuotaBlock = &QuotaBlock{KeyHash: quotaKeyHash(c), Source: "usage", BlockedAt: now, ObservedAt: now, Limits: []QuotaBlockLimit{{Type: "weekly"}}}
	quotaTestAuth(t, s, c)
	quotaTestAuth(t, s, Credential{ID: "manual", APIKey: "manual-key", Disabled: true})
	s.SetHost(func(method string, _, out any) error {
		if method != "host.auth.list" {
			t.Fatalf("unexpected scheduling callback: %s", method)
		}
		return json.Unmarshal([]byte(`{"files":[{"name":"quota.json","provider":"cline-pass","disabled":true,"status":"disabled"},{"name":"manual.json","provider":"cline-pass","disabled":true,"status":"disabled"}]}`), out)
	})
	result, err := s.schedulingResponse()
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		QuotaAutoPause bool `json:"quota_auto_pause"`
		Accounts       []struct {
			ID           string `json:"id"`
			Enabled      bool   `json:"enabled"`
			QuotaBlocked bool   `json:"quota_blocked"`
			QuotaReason  string `json:"quota_reason"`
			Host         struct {
				Disabled bool `json:"disabled"`
			} `json:"host"`
		} `json:"accounts"`
		Host struct {
			Available bool `json:"available"`
		} `json:"host"`
	}
	response := result.(ManagementResponse)
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &state) != nil || !state.QuotaAutoPause || !state.Host.Available || len(state.Accounts) != 2 {
		t.Fatalf("invalid scheduling state: %s", response.Body)
	}
	for _, account := range state.Accounts {
		if !account.Host.Disabled {
			t.Fatal("account host runtime disabled state was dropped")
		}
		if account.ID == c.ID {
			if !account.Enabled || !account.QuotaBlocked || account.QuotaReason != "每周额度耗尽" {
				t.Fatal("quota projection was confused with manual disable")
			}
		} else if account.Enabled || account.QuotaBlocked {
			t.Fatal("manual disable was confused with quota pause")
		}
	}
	if s.creds[c.ID].Disabled || !s.creds["manual"].Disabled {
		t.Fatal("reading host runtime state changed manual enable/disable")
	}
}

func TestSchedulingCredentialManualTogglePreservesQuotaBlock(t *testing.T) {
	s := NewService()
	c := Credential{ID: "one", APIKey: "quota-key"}
	now := time.Now().UTC()
	c.QuotaBlock = &QuotaBlock{KeyHash: quotaKeyHash(c), Source: "usage", BlockedAt: now, ObservedAt: now, Limits: []QuotaBlockLimit{{Type: "weekly"}}}
	quotaTestAuth(t, s, c)
	for _, enabled := range []bool{false, true} {
		result, err := s.schedulingSetCredential(ManagementRequest{Body: jsonBytes(map[string]any{"id": c.ID, "enabled": enabled})})
		if err != nil || result.(ManagementResponse).StatusCode != 200 {
			t.Fatalf("manual toggle failed: %v", err)
		}
		current := s.creds[c.ID]
		if current.Disabled != !enabled || current.QuotaBlock == nil {
			t.Fatal("manual toggle erased the independent quota block")
		}
		if quotaTestParse(t, s, c.ID, quotaTestRead(t, s, c.ID))["Disabled"] != true || s.creds[c.ID].Disabled != !enabled {
			t.Fatal("host quota projection rewrote manual enable/disable")
		}
	}
}

func TestManagementRequestsHonorCredentialProxyWithoutSharedPool(t *testing.T) {
	var proxyCalls, hostHTTP atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer proxy-fixture-key" {
			t.Error("missing origin credential")
		}
		if r.Header.Get("Proxy-Authorization") == "" {
			t.Error("missing proxy authentication")
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(simpleSSE())
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"limits":[]}}`))
		}
	}))
	defer proxy.Close()
	s := probeService(t)
	s.cfg.BaseURL = "http://cline.fixture.invalid/v1"
	c := s.creds["probe-account"]
	c.APIKey, c.ProxyURL = "proxy-fixture-key", strings.Replace(proxy.URL, "http://", "http://proxy-user:proxy-password@", 1)
	s.creds[c.ID] = c
	s.SetHost(func(method string, _, _ any) error {
		if strings.HasPrefix(method, "host.http.") {
			hostHTTP.Add(1)
		}
		return errors.New("unsupported callback")
	})
	if _, err := s.usageGET("", c, "/usage-limits", time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	result := callModelTest(t, s)
	if !strings.Contains(string(result.Body), `"ok":true`) {
		t.Fatalf("proxied probe failed: %s", result.Body)
	}
	if proxyCalls.Load() != 2 || hostHTTP.Load() != 0 {
		t.Fatalf("proxy=%d host=%d", proxyCalls.Load(), hostHTTP.Load())
	}
}
