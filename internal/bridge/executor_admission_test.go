package bridge

import (
	"errors"
	"testing"
	"time"
)

func TestExecutorLocal429ReturnsDirectly(t *testing.T) {
	for _, scope := range []string{"team", "egress"} {
		for _, method := range []string{"executor.execute", "executor.execute_stream"} {
			t.Run(scope+"/"+method, func(t *testing.T) {
				s := registeredService(t, "")
				host := newFakeHost()
				s.SetHost(func(method string, in, out any) error {
					t.Errorf("local rejection invoked host callback %s", method)
					return host.call(method, in, out)
				})
				var retryAt time.Time
				if scope == "team" {
					c := Credential{Type: Provider, ID: "credential-1", APIKey: "test-key"}
					lease, err := s.rateLimits.acquire(c, "cline-pass/deepseek-v4.1-flash")
					if err != nil {
						t.Fatal(err)
					}
					retryAt = time.Now().Add(time.Minute)
					lease.finish(&upstreamError{APIError: &APIError{429, "upstream_team_rate_limited", "team"}, RetryAt: retryAt, Scope: "upstream_team"})
				} else {
					cfg := defaultEgressConfig()
					cfg.Enabled, cfg.WaitTimeoutSeconds = true, 0
					if _, err := s.saveEgressConfig(jsonBytes(cfg)); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { s.getEgressManager().close() })
				}
				done := make(chan error, 1)
				go func() { _, err := s.Handle(method, executorRequest("client")); done <- err }()
				var err error
				select {
				case err = <-done:
				case <-time.After(time.Second):
					t.Fatal("local rejection delayed the executor RPC response")
				}
				var detail *upstreamError
				if !errors.As(err, &detail) || statusOf(err) != 429 || !detail.Local || detail.RetryAt.IsZero() {
					t.Fatalf("local rejection lost its classification or retry information: %v", err)
				}
				if scope == "team" && (detail.Kind != "upstream_team_rate_limited" || !detail.RetryAt.Equal(retryAt)) {
					t.Fatalf("team cooldown changed: %+v", detail)
				}
				if scope == "egress" && detail.Kind != "local_egress_unavailable" {
					t.Fatalf("egress rejection changed: %+v", detail)
				}
			})
		}
	}
}
