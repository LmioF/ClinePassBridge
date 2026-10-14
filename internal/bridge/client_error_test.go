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
		if h.clientError != want {
			t.Fatalf("compatibility=%v: stream close %q, want %q", enabled, h.clientError, want)
		}
		if !err.(*upstreamError).IsRequestScoped() || err.Error() != transportIncompleteFixture {
			t.Fatal("compatibility toggle changed scheduling or diagnostic error")
		}
	}
}

func TestClientErrorTransportMessage(t *testing.T) {
	err := classifyUpstreamError(200, nil, map[string]any{"error": transportIncompleteFixture}, time.Now())
	message := ClientErrorMessage(err)
	if !strings.Contains(message, "502") || strings.Contains(message, "400") || strings.Contains(message, "invalid request") {
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

func TestClientErrorKeepsOtherFaults(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 429, 502, 504} {
		err := classifyUpstreamError(status, nil, map[string]any{"error": "original upstream error"}, time.Now())
		if ClientErrorMessage(err) != safeError(err) {
			t.Fatalf("ordinary status %d was rewritten", status)
		}
	}
	if ClientErrorMessage(nil) != "" {
		t.Fatal("successful close acquired an error")
	}
}
