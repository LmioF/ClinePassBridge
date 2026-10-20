package bridge

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientErrorCompatibilityPersistsAndControlsStreamClose(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	err := classifyUpstreamError(200, nil, map[string]any{"error": transportIncompleteFixture}, time.Now())
	if !s.config().ClientRetryCompatibility {
		t.Fatal("existing settings without the new field must enable compatibility")
	}
	h := newFakeHost()
	s.SetHost(h.call)
	for _, enabled := range []bool{false, true} {
		observabilityManagement(t, s, "PUT", "/config", nil, jsonBytes(map[string]any{"client_retry_compatibility": enabled}), 200)
		observabilityManagement(t, s, "PUT", "/config", nil, []byte(`{"log_retention":500}`), 200)
		configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(s.config().DataDir))
		if reloadErr := s.configure(jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); reloadErr != nil {
			t.Fatal(reloadErr)
		}
		if s.config().ClientRetryCompatibility != enabled {
			t.Fatal("saved setting was lost after partial update or reconfiguration")
		}
		want := transportIncompleteFixture
		if enabled {
			want = ClientErrorMessage(err)
		}
		if got := s.ClientErrorMessage(err); got != want {
			t.Fatalf("compatibility=%v: client error %q, want %q", enabled, got, want)
		}
		s.closeOutputStream("client-stream", &LogEntry{ErrorPhase: "after_output"}, err)
		wantStream := want
		if !enabled {
			wantStream += ": unexpected EOF"
		}
		if h.clientError != wantStream {
			t.Fatalf("compatibility=%v: stream close %q, want %q", enabled, h.clientError, wantStream)
		}
		if !err.(*upstreamError).IsRequestScoped() || err.Error() != transportIncompleteFixture {
			t.Fatal("compatibility toggle changed scheduling or diagnostic error")
		}
	}
}

func TestClientErrorTransportMessage(t *testing.T) {
	err := classifyUpstreamError(200, nil, map[string]any{"error": transportIncompleteFixture}, time.Now())
	message := ClientErrorMessage(err)
	if !strings.Contains(message, "502") || !strings.Contains(message, "unexpected EOF") || strings.Contains(message, "400") || strings.Contains(message, "invalid request") {
		t.Fatalf("nested provider status leaked into client fault: %q", message)
	}
	if err.Error() != transportIncompleteFixture {
		t.Fatal("original diagnostic exception changed")
	}
	var entry LogEntry
	logErrorDetails(&entry, nil, err)
	entry.Error = safeError(err)
	if entry.Error != transportIncompleteFixture || entry.ErrorKind != transportIncompleteKind {
		t.Fatalf("original diagnostic log lost: %#v", entry)
	}
}

func TestStreamCooldownMarkersPreserveOtherFaults(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	h := newFakeHost()
	s.SetHost(h.call)
	for _, enabled := range []bool{false, true} {
		s.cfg.ClientRetryCompatibility = enabled
		for _, status := range []int{401, 402, 403, 429} {
			err := classifyUpstreamError(status, nil, map[string]any{"error": "original upstream error"}, time.Now())
			s.closeOutputStream("client-stream", &LogEntry{}, err)
			if h.clientError != safeError(err) {
				t.Fatalf("compatibility=%v status=%d changed unrelated fault: %q", enabled, status, h.clientError)
			}
		}
		// Actual auth/quota/rate failures win even if the provider body resembles
		// the known LiteLLM truncation signature.
		for _, status := range []int{401, 402, 403, 429} {
			err := classifyUpstreamError(status, nil, map[string]any{"error": transportIncompleteFixture}, time.Now())
			s.closeOutputStream("client-stream", &LogEntry{}, err)
			if h.clientError != safeError(err) {
				t.Fatalf("compatibility=%v status=%d acquired a transport exemption", enabled, status)
			}
		}
		err := fmt.Errorf("wrapped: %w", streamIdleTimeout(10*time.Second))
		s.closeOutputStream("client-stream", &LogEntry{}, err)
		if strings.Count(h.clientError, "i/o timeout") != 1 || !strings.Contains(h.clientError, "HTTP 502:") {
			t.Fatalf("compatibility=%v: idle error lost marker or status: %q", enabled, h.clientError)
		}
		s.closeOutputStream("client-stream", &LogEntry{}, nil)
		if h.clientError != "" {
			t.Fatal("successful close acquired an error")
		}
	}
}

func TestHostErrorDetailsAvoidsCooldownBeforeStreamHandoff(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	for _, enabled := range []bool{false, true} {
		s.cfg.ClientRetryCompatibility = enabled
		for _, err := range []error{
			classifyUpstreamError(200, nil, map[string]any{"error": transportIncompleteFixture}, time.Now()),
			fmt.Errorf("wrapped: %w", streamIdleTimeout(10*time.Second)),
		} {
			message, status := s.HostErrorDetails(err)
			if status != 0 || (!strings.Contains(message, "unexpected EOF") && !strings.Contains(message, "i/o timeout")) {
				t.Fatalf("compatibility=%v: RPC fault blocks stock transport classification: status=%d message=%q", enabled, status, message)
			}
			if statusOf(err) != 502 {
				t.Fatal("wire compatibility changed diagnostic status")
			}
		}
		for _, status := range []int{401, 402, 403, 429} {
			err := classifyUpstreamError(status, nil, map[string]any{"error": "ordinary upstream error"}, time.Now())
			message, gotStatus := s.HostErrorDetails(err)
			if gotStatus != status || message != safeError(err) {
				t.Fatalf("unrelated RPC error changed: status=%d message=%q", gotStatus, message)
			}
		}
		for _, status := range []int{502, 504} {
			err := fail(status, "unclassified local failure")
			message, gotStatus := s.HostErrorDetails(err)
			if gotStatus != 0 || !strings.Contains(message, "[clinepassbridge:cpa_compat:server misbehaving]") {
				t.Fatalf("unclassified fault missed default exemption: status=%d message=%q", gotStatus, message)
			}
		}
	}
}

func TestClientErrorKeepsOtherFaults(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 429} {
		err := classifyUpstreamError(status, nil, map[string]any{"error": "original upstream error"}, time.Now())
		if ClientErrorMessage(err) != safeError(err) {
			t.Fatalf("ordinary status %d was rewritten", status)
		}
	}
	if ClientErrorMessage(nil) != "" {
		t.Fatal("successful close acquired an error")
	}
}
