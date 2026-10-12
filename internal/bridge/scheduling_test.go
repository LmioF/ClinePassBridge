package bridge

import (
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
	s.SetHost(func(string, any, any) error { return errors.New("unsupported callback") })
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
	if !strings.Contains(string(result.(ManagementResponse).Body), `"local_delay_supported":false`) {
		t.Fatal("old host capabilities overstated")
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
