package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEgressChallengeCoolsSharedIPWithoutChangingCredential(t *testing.T) {
	var calls, mode atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Cf-Ray", "fixture-edge")
		if mode.Load() == 0 {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<!doctype html><html><title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script></html>`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"permission_denied","message":"credential denied; cloudflare challenge is unrelated"}}`)
	}))
	defer origin.Close()
	s := egressTestService(t, directTestEndpoint("a", "shared", 60, 2), directTestEndpoint("b", "shared", 60, 2))
	var request ExecutorRequest
	if err := json.Unmarshal(executorRequest(""), &request); err != nil {
		t.Fatal(err)
	}
	var credential Credential
	if err := json.Unmarshal(request.StorageJSON, &credential); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.BaseURL, s.cfg.NonstreamMode = origin.URL, "native"
	s.creds[credential.ID] = credential
	s.mu.Unlock()
	before := jsonBytes(credential)
	p := s.getEgressManager()
	now := time.Now().Truncate(time.Second)
	p.clock = func() time.Time { return now }
	_, err := s.execute(request)
	var detail *upstreamError
	if !errors.As(err, &detail) || detail.Kind != upstreamEdgeKind || detail.Status != 502 || detail.HTTPStatus != 403 || detail.UpstreamStatus != 403 || detail.Scope != "egress_ip" {
		t.Fatalf("challenge lost its egress classification or HTTP evidence: %#v, %v", detail, err)
	}
	if !detail.RetryAt.Equal(now.Add(120 * time.Second)) {
		t.Fatalf("challenge ignored Retry-After: %v", detail.RetryAt)
	}
	if policy := faultPolicyFor(err); policy.Cooldown || policy.Stop {
		t.Fatalf("challenge penalized the credential or stopped route fallback: %+v", policy)
	}
	s.mu.RLock()
	after := jsonBytes(s.creds[credential.ID])
	s.mu.RUnlock()
	if !bytes.Equal(before, after) {
		t.Fatal("challenge changed the stored credential state")
	}
	s.rateLimits.mu.Lock()
	windows := len(s.rateLimits.windows)
	s.rateLimits.mu.Unlock()
	if windows != 0 {
		t.Fatal("challenge opened an account/model rate window")
	}
	if _, err := s.openUpstream(egressTestRequest(origin.URL), time.Now().Add(time.Second)); statusOf(err) != 429 || calls.Load() != 1 {
		t.Fatal("shared-IP cooldown allowed another endpoint or replayed the request", err)
	}
	p.mu.Lock()
	p.groups["shared"].cooldown = time.Time{}
	p.mu.Unlock()
	mode.Store(1)
	_, err = s.execute(request)
	if !errors.As(err, &detail) || detail.Kind != "upstream_auth_error" || detail.Status != 403 || !faultPolicyFor(err).Cooldown {
		t.Fatal("JSON authentication error acquired an edge exemption", err)
	}
	p.mu.Lock()
	cooldown := p.groups["shared"].cooldown
	p.mu.Unlock()
	if !cooldown.IsZero() || calls.Load() != 2 {
		t.Fatal("JSON authentication error opened an IP cooldown or caused a replay")
	}
}

func TestEgressChallengeRetryWindowAndHostPath(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name, retryAfter string
		prior, want      time.Duration
	}{
		{name: "configured", want: 5 * time.Minute},
		{name: "invalid-header", retryAfter: "invalid", want: 5 * time.Minute},
		{name: "numeric-header", retryAfter: "90", want: 90 * time.Second},
		{name: "date-header", retryAfter: now.Add(2 * time.Minute).UTC().Format(http.TimeFormat), want: 2 * time.Minute},
		{name: "preserve-longer-window", retryAfter: "90", prior: 10 * time.Minute, want: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newEgressManager()
			p.clock = func() time.Time { return now }
			cfg := defaultEgressConfig()
			cfg.Enabled, cfg.WaitTimeoutSeconds, cfg.IPCooldownSeconds = true, 0, 300
			cfg.Endpoints = []EgressEndpoint{directTestEndpoint("a", "shared", 60, 2)}
			p.apply(cfg)
			lease, err := p.acquire(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer lease.release()
			if tc.prior > 0 {
				lease.group.cooldown = now.Add(tc.prior)
			}
			original := &upstreamError{APIError: &APIError{Status: 502, Kind: upstreamEdgeKind, Message: "edge challenge"}, HTTPStatus: 403, UpstreamStatus: 403, Scope: "egress_ip"}
			up := upstreamStream{StatusCode: 403, Headers: http.Header{"Retry-After": {tc.retryAfter}}, local: &egressLocalStream{pool: p, lease: lease}}
			s := NewService()
			classified := s.classifyEgressError(up, nil, nil, original)
			var detail *upstreamError
			if !errors.As(classified, &detail) || detail.APIError != original.APIError || detail.HTTPStatus != 403 || detail.UpstreamStatus != 403 || detail.Scope != "egress_ip" || !detail.RetryAt.Equal(now.Add(tc.want)) || !detail.RetryAt.Equal(lease.group.cooldown) {
				t.Fatalf("challenge retry window or classification changed: %#v", detail)
			}
			if got := s.classifyEgressError(upstreamStream{}, nil, nil, original); got != original {
				t.Fatal("host transport acquired a local-pool error replacement")
			}
		})
	}
}

type egressPolicyRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip egressPolicyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestEgressLifecycleErrorsStayRequestScoped(t *testing.T) {
	assertUnavailable := func(t *testing.T, err error) {
		t.Helper()
		var detail *upstreamError
		if !errors.As(err, &detail) || detail.Status != 503 || detail.Kind != bridgeUnavailableKind || !detail.IsRequestScoped() || faultPolicyFor(err).Cooldown {
			t.Fatalf("lifecycle closure lost its credential exemption: %#v, %v", detail, err)
		}
	}
	t.Run("closed-admission", func(t *testing.T) {
		p := newEgressManager()
		p.close()
		_, err := p.acquire(context.Background(), "")
		assertUnavailable(t, err)
		_, err = p.operation("fixture", time.Now().Add(time.Second))
		assertUnavailable(t, err)
	})
	for _, mode := range []string{"pool-closed-before-open", "pool-closes-with-response", "service-stops-with-response"} {
		t.Run(mode, func(t *testing.T) {
			s := egressTestService(t, directTestEndpoint("a", "shared", 60, 2))
			p := s.getEgressManager()
			if mode == "pool-closed-before-open" {
				p.close()
			} else {
				p.client("direct").Transport = egressPolicyRoundTripper(func(r *http.Request) (*http.Response, error) {
					if mode == "pool-closes-with-response" {
						p.close()
					} else {
						s.mu.Lock()
						s.stopped = true
						s.mu.Unlock()
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: r}, nil
				})
			}
			_, err := s.openUpstream(egressTestRequest("http://fixture.invalid/resource"), time.Now().Add(time.Second))
			assertUnavailable(t, err)
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.operations) != 0 || len(p.streams) != 0 {
				t.Fatal("lifecycle race retained operations or streams")
			}
		})
	}
}
