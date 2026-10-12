package bridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func egressTestService(t *testing.T, endpoints ...EgressEndpoint) *Service {
	t.Helper()
	s := registeredService(t, "")
	cfg := defaultEgressConfig()
	cfg.Enabled, cfg.WaitTimeoutSeconds, cfg.Endpoints = true, 0, endpoints
	if _, err := s.saveEgressConfig(jsonBytes(cfg)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.getEgressManager().close() })
	return s
}

func directTestEndpoint(id, group string, rpm, inflight int) EgressEndpoint {
	return EgressEndpoint{ID: id, Group: group, ProxyURL: "direct", Enabled: true, RPM: rpm, MaxInflight: inflight}
}

func egressTestRequest(target string) map[string]any {
	return map[string]any{"method": "GET", "url": target, "headers": http.Header{}}
}

func TestEgressConfigMasksPreservesAndReloadsSecrets(t *testing.T) {
	s := registeredService(t, "")
	secret := "private-fixture-password"
	cfg := defaultEgressConfig()
	cfg.Endpoints = []EgressEndpoint{{ID: "one", Enabled: true, Group: "ip-one", ProxyURL: "socks5h://fixture-user:" + secret + "@localhost:1080", RPM: 60, MaxInflight: 4}}
	view, err := s.saveEgressConfig(jsonBytes(cfg))
	if err != nil || strings.Contains(string(jsonBytes(view)), secret) || strings.Contains(string(jsonBytes(view)), "fixture-user") || !view.Endpoints[0].HasAuth {
		t.Fatal("proxy authentication was not safely masked", err)
	}
	for _, blank := range []bool{false, true} {
		if blank {
			view.Endpoints[0].ProxyURL = ""
		}
		view, err = s.saveEgressConfig(jsonBytes(view))
		if err != nil {
			t.Fatal("masked or blank URL failed to preserve credentials", err)
		}
	}
	disk, err := os.ReadFile(filepath.Join(s.config().DataDir, "egress.json"))
	if err != nil || !strings.Contains(string(disk), secret) {
		t.Fatal("private configuration did not retain its credentials", err)
	}
	reloaded := NewService()
	if _, err := reloaded.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(s.config().DataDir)))})); err != nil {
		t.Fatal(err)
	}
	loaded, _ := reloaded.egressConfigResponse()
	if string(jsonBytes(loaded)) != string(jsonBytes(view)) {
		t.Fatal("egress reload changed the public configuration")
	}
	view.Endpoints = append(view.Endpoints, EgressEndpoint{ID: "two", Enabled: true, Group: "ip-one", ProxyURL: "direct", RPM: 50, MaxInflight: 4})
	if _, err := s.saveEgressConfig(jsonBytes(view)); statusOf(err) != 400 {
		t.Fatal("inconsistent limits in one physical group were accepted")
	}
	if after, _ := os.ReadFile(filepath.Join(s.config().DataDir, "egress.json")); string(after) != string(disk) {
		t.Fatal("a rejected update changed the persisted configuration")
	}
}

func TestEgressOptionalHostContextAndQuiescedRecovery(t *testing.T) {
	started := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer origin.Close()
	s := egressTestService(t, directTestEndpoint("direct", "ip", 60, 4))
	var disconnected atomic.Bool
	var capabilityCalls atomic.Int32
	s.SetHost(func(method string, payload, out any) error {
		var response any
		switch method {
		case "host.scheduling.capabilities":
			capabilityCalls.Add(1)
			response = map[string]any{"request_context_supported": true, "local_delay_supported": true}
		case "host.request.context":
			if str(payload.(map[string]any)["host_callback_id"]) != "host-callback" {
				return errors.New("wrong host callback ID")
			}
			state := "active"
			if disconnected.Load() {
				state = "canceled"
			}
			response = map[string]any{"supported": true, "state": state}
		default:
			return fmt.Errorf("unexpected host callback %s", method)
		}
		return json.Unmarshal(jsonBytes(response), out)
	})
	payload := egressTestRequest(origin.URL + "/block")
	payload["host_callback_id"] = "host-callback"
	result := make(chan error, 1)
	go func() {
		_, err := s.openUpstream(payload, time.Now().Add(3*time.Second))
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native request did not reach the fixture")
	}
	disconnected.Store(true)
	select {
	case err := <-result:
		if statusOf(err) != 499 {
			t.Fatal("optional host cancellation did not cancel native HTTP", err)
		}
	case <-time.After(time.Second):
		t.Fatal("optional host cancellation did not promptly terminate native HTTP")
	}
	if capabilityCalls.Load() != 1 || s.egressStatus()["request_context_supported"] != true {
		t.Fatal("capability discovery did not use the scheduling callback")
	}
	old := s.getEgressManager()
	if _, err := s.Handle("plugin.quiesce", nil); err != nil {
		t.Fatal(err)
	}
	if !old.closed {
		t.Fatal("quiesce did not close the old egress manager")
	}
	configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(s.config().DataDir))
	if _, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); err != nil {
		t.Fatal("failed-replacement rollback could not re-register the old instance", err)
	}
	if s.getEgressManager() == old || !s.egressEnabled() {
		t.Fatal("re-registration did not rebuild and reload the egress manager")
	}
	up, err := s.openUpstream(egressTestRequest(origin.URL), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal("native transport remained closed after re-registration", err)
	}
	if _, err := s.readJSON(up); err != nil {
		t.Fatal(err)
	}
}

func TestEgressPoolSharesRollingRPMAndInflight(t *testing.T) {
	p := newEgressManager()
	now := time.Now()
	p.clock = func() time.Time { return now }
	cfg := defaultEgressConfig()
	cfg.Enabled, cfg.WaitTimeoutSeconds = true, 0
	cfg.Endpoints = []EgressEndpoint{directTestEndpoint("a", "same-ip", 2, 1), directTestEndpoint("b", "same-ip", 2, 1)}
	p.apply(cfg)
	lease, err := p.acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.acquire(context.Background(), ""); statusOf(err) != 429 {
		t.Fatal("two URLs bypassed their shared in-flight limit")
	}
	p.mu.Lock()
	lease.sent = true
	p.mu.Unlock()
	lease.release()
	second, err := p.acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	second.sent = true
	p.mu.Unlock()
	second.release()
	p.mu.Lock()
	now = now.Add(59 * time.Second)
	p.mu.Unlock()
	if _, err := p.acquire(context.Background(), ""); statusOf(err) != 429 {
		t.Fatal("rolling RPM reopened before 60 seconds")
	}
	// Editing an unchanged pool must not erase the existing RPM window.
	p.apply(cfg)
	if _, err := p.acquire(context.Background(), ""); statusOf(err) != 429 {
		t.Fatal("saving the configuration erased the RPM window")
	}
	p.mu.Lock()
	now = now.Add(time.Second)
	p.mu.Unlock()
	third, err := p.acquire(context.Background(), "")
	if err != nil {
		t.Fatal("rolling window did not reopen at 60 seconds", err)
	}
	third.release()
}

func TestEgressPoolWaitWakesAndCancels(t *testing.T) {
	p := newEgressManager()
	cfg := defaultEgressConfig()
	cfg.Enabled = true
	cfg.Endpoints = []EgressEndpoint{directTestEndpoint("one", "one", 60, 1)}
	p.apply(cfg)
	first, _ := p.acquire(context.Background(), "")
	done := make(chan error, 1)
	go func() { lease, err := p.acquire(context.Background(), ""); lease.release(); done <- err }()
	first.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a completed response did not wake the waiter")
	}
	first, _ = p.acquire(context.Background(), "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.acquire(ctx, ""); statusOf(err) != 499 {
		t.Fatal("canceled queue admission did not return 499")
	}
	first.release()
	cfg.WaitTimeoutSeconds = 0
	cfg.Endpoints = nil
	p.apply(cfg)
	start := time.Now()
	_, err := p.acquire(context.Background(), "")
	var local *upstreamError
	if !errors.As(err, &local) || !local.Local || statusOf(err) != 429 || time.Since(start) > time.Second {
		t.Fatal("an empty enabled pool did not return a bounded local 429")
	}
}

func TestEgressHTTPConnectionReuseAndCleanup(t *testing.T) {
	var connections atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy authentication reached the origin")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	origin.Start()
	defer origin.Close()
	s := egressTestService(t, directTestEndpoint("one", "one", 60, 2))
	for n := 0; n < 2; n++ {
		request := egressTestRequest(origin.URL)
		request["headers"] = http.Header{"Proxy-Authorization": []string{"must-not-reach-origin"}}
		up, err := s.openUpstream(request, time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.readJSON(up); err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 1 {
		t.Fatal("local transport did not reuse the HTTP connection")
	}
	p := s.getEgressManager()
	p.mu.Lock()
	if len(p.streams) != 0 || len(p.operations) != 0 || p.groups["one"].inflight != 0 || len(p.clients) != 1 {
		p.mu.Unlock()
		t.Fatal("completed HTTP responses leaked streams or admissions")
	}
	p.mu.Unlock()
	p.close()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clients) != 0 {
		t.Fatal("close did not retire cached clients")
	}
}

func TestEgressHTTPSProxyAndCredentialPriority(t *testing.T) {
	var seenAuth atomic.Bool
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(strings.HasPrefix(r.Header.Get("Proxy-Authorization"), "Basic "))
		_, _ = io.WriteString(w, `{"route":"proxy"}`)
	}))
	defer proxy.Close()
	proxyURL := strings.Replace(proxy.URL, "https://", "https://proxy-user:fixture-secret@", 1)
	s := egressTestService(t, directTestEndpoint("direct", "direct", 60, 4))
	transport := s.getEgressManager().client(proxyURL).Transport.(*http.Transport)
	transport.TLSClientConfig = proxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	request := s.egressCredentialPayload(egressTestRequest("http://origin.invalid/resource"), Credential{ProxyURL: proxyURL}, true)
	up, err := s.openUpstream(request, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	body, err := s.readJSON(up)
	if err != nil || !strings.Contains(string(body), "proxy") || !seenAuth.Load() {
		t.Fatal("credential HTTPS proxy was not used with authentication", err)
	}
	bad := s.egressCredentialPayload(egressTestRequest("http://origin.invalid/resource"), Credential{ProxyURL: "http://user:secret-marker@127.0.0.1:1"}, true)
	_, err = s.openUpstream(bad, time.Now().Add(time.Second))
	if statusOf(err) != 502 || strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatal("a failed proxy leaked authentication or silently changed routes", err)
	}
}

func TestEgressHTTPIPCooldownLeavesQuotaAndUnknownAlone(t *testing.T) {
	var mode atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 0 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, "<!doctype html><title>429</title>Too Many Requests")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		if mode.Load() == 1 {
			_, _ = io.WriteString(w, `{"error":{"code":"quota_exceeded","message":"weekly quota exceeded"}}`)
		} else {
			_, _ = io.WriteString(w, `{"error":{"message":"busy"}}`)
		}
	}))
	defer origin.Close()
	s := egressTestService(t, directTestEndpoint("a", "shared", 60, 2), directTestEndpoint("b", "shared", 60, 2))
	up, _ := s.openUpstream(egressTestRequest(origin.URL), time.Now().Add(time.Second))
	_, err := s.readJSON(up)
	var detail *upstreamError
	if !errors.As(err, &detail) || detail.Kind != "upstream_ip_rate_limited" || detail.Local {
		t.Fatal("HTML edge 429 was not classified as a real IP limit", err)
	}
	if _, err := s.openUpstream(egressTestRequest(origin.URL), time.Now().Add(time.Second)); statusOf(err) != 429 {
		t.Fatal("a second URL bypassed the physical IP cooldown")
	}
	p := s.getEgressManager()
	for _, which := range []int32{1, 2} {
		p.mu.Lock()
		p.groups["shared"].cooldown = time.Time{}
		p.mu.Unlock()
		mode.Store(which)
		up, _ = s.openUpstream(egressTestRequest(origin.URL), time.Now().Add(time.Second))
		_, err = s.readJSON(up)
		if !errors.As(err, &detail) || detail.Kind == "upstream_ip_rate_limited" {
			t.Fatal("quota/unknown JSON 429 became an IP limit", err)
		}
		p.mu.Lock()
		limited := p.groups["shared"].cooldown.After(time.Now())
		p.mu.Unlock()
		if limited {
			t.Fatal("quota/unknown JSON 429 opened an egress cooldown")
		}
	}
}

func TestEgressCancellationDeadlineAndClose(t *testing.T) {
	started := make(chan struct{}, 4)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer origin.Close()
	for _, mode := range []string{"cancel-api", "bound-context", "deadline", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			s := egressTestService(t, directTestEndpoint("one", "one", 60, 4))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "bound-context" {
				defer s.SetEgressRequestContext("callback", ctx)()
			}
			request := egressTestRequest(origin.URL)
			request["host_callback_id"] = "callback"
			deadline := time.Now().Add(time.Second)
			if mode == "deadline" {
				deadline = time.Now().Add(120 * time.Millisecond)
			}
			done := make(chan error, 1)
			go func() { _, err := s.openUpstream(request, deadline); done <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
			want := 499
			switch mode {
			case "cancel-api":
				s.CancelEgressRequest("callback")
			case "bound-context":
				cancel()
			case "deadline":
				want = 504
			case "shutdown":
				s.getEgressManager().close()
			}
			select {
			case err := <-done:
				if statusOf(err) != want {
					t.Fatalf("cancel/deadline status=%d want=%d", statusOf(err), want)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation/deadline did not stop the pending HTTP request")
			}
		})
	}
}

func TestEgressDisabledRetainsHostPathAndManagementOptIn(t *testing.T) {
	s := registeredService(t, "")
	h := newFakeHost(ssePlan(simpleSSE()))
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.http.do_stream" {
			v := payload.(map[string]any)
			if _, exists := v["egress_proxy_url"]; exists {
				t.Error("private egress fields changed the unconfigured host payload")
			}
		}
		return h.call(method, payload, out)
	})
	var request ExecutorRequest
	_ = json.Unmarshal(executorRequest(""), &request)
	request.StorageJSON = jsonBytes(Credential{Type: Provider, ID: "credential-1", APIKey: "test-key", ProxyURL: "http://old-host-proxy:8080"})
	if _, err := s.execute(request); err != nil || len(h.opened) != 1 {
		t.Fatal("disabled pool changed the original host HTTP path", err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) }))
	defer proxy.Close()
	payload := s.egressCredentialPayload(egressTestRequest("http://origin.invalid/resource"), Credential{ProxyURL: proxy.URL}, true)
	up, err := s.openUpstream(payload, time.Now().Add(time.Second))
	if err != nil || up.local == nil {
		t.Fatal("management credential proxy did not opt into native transport", err)
	}
	if _, err := s.readJSON(up); err != nil {
		t.Fatal(err)
	}
	s.getEgressManager().close()
}

func TestEgressSOCKSRemoteDNSAndAuthentication(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) }))
	defer origin.Close()
	destination, _ := url.Parse(origin.URL)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	domains := make(chan string, 2)
	var workers sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				header := make([]byte, 2)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				methods := make([]byte, int(header[1]))
				_, _ = io.ReadFull(conn, methods)
				_, _ = conn.Write([]byte{5, 2})
				_, _ = io.ReadFull(conn, header)
				username := make([]byte, int(header[1]))
				_, _ = io.ReadFull(conn, username)
				length := []byte{0}
				_, _ = io.ReadFull(conn, length)
				password := make([]byte, int(length[0]))
				_, _ = io.ReadFull(conn, password)
				if string(username) != "fixture-user" || string(password) != "fixture-password" {
					_, _ = conn.Write([]byte{1, 1})
					return
				}
				_, _ = conn.Write([]byte{1, 0})
				header = make([]byte, 4)
				_, _ = io.ReadFull(conn, header)
				if header[3] != 3 {
					return // The unresolved hostname must reach the SOCKS gateway.
				}
				_, _ = io.ReadFull(conn, length)
				host := make([]byte, int(length[0]))
				_, _ = io.ReadFull(conn, host)
				port := make([]byte, 2)
				_, _ = io.ReadFull(conn, port)
				_ = binary.BigEndian.Uint16(port)
				domains <- string(host)
				upstream, err := net.DialTimeout("tcp", destination.Host, time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close() }()
				_, _ = io.Copy(conn, upstream)
			}()
		}
	}()
	for _, scheme := range []string{"socks5", "socks5h"} {
		s := egressTestService(t, EgressEndpoint{ID: "socks", Group: "socks", Enabled: true, ProxyURL: scheme + "://fixture-user:fixture-password@" + listener.Addr().String(), RPM: 60, MaxInflight: 4})
		up, err := s.openUpstream(egressTestRequest("http://origin.invalid/resource"), time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.readJSON(up); err != nil {
			t.Fatal(err)
		}
		if domain := <-domains; domain != "origin.invalid" {
			t.Fatal("SOCKS destination was resolved locally")
		}
		s.getEgressManager().close()
	}
	workers.Wait()
}
