package bridge

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type networkLogsView struct {
	Items   []LogEntry `json:"items"`
	Total   int        `json:"total"`
	Offset  int        `json:"offset"`
	Summary struct {
		Requests         int64 `json:"requests"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		CachedTokens     int64 `json:"cached_tokens"`
	} `json:"summary"`
}

func networkLogs(t *testing.T, s *Service, query url.Values) networkLogsView {
	t.Helper()
	response := observabilityManagement(t, s, "GET", "/logs", query, nil, 200)
	var view networkLogsView
	if err := json.Unmarshal(response.Body, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func TestNetworkLogFilterBeforePaginationAndSummary(t *testing.T) {
	s := observabilityService(t, "")
	rows := []LogEntry{
		{ID: "legacy", Status: 200, Error: "exa_search was mentioned in content"},
		{ID: "used-first", Status: 200, Provider: "first", PromptTokens: 10, CompletionTokens: 5, CachedTokens: 4,
			ServerTools: []string{"vercel:exa_search"}, ServerToolCalls: map[string]int64{"exa_search": 9}, ServerToolCallsReported: true},
		// Actual usage qualifies even without a matching declaration in the log.
		{ID: "used-second", Status: 502, Provider: "second", PromptTokens: 20, CompletionTokens: 6, CachedTokens: 8,
			ServerToolCalls: map[string]int64{"perplexity_search": 1}, ServerToolCallsReported: true},
		{ID: "zero", Status: 200, ServerTools: []string{"vercel:exa_search"},
			ServerToolCalls: map[string]int64{"exa_search": 0}, ServerToolCallsReported: true},
		{ID: "unreported", Status: 200, ServerTools: []string{"vercel:exa_search"}},
		{ID: "empty-counts", Status: 200, ServerToolCalls: map[string]int64{}, ServerToolCallsReported: true},
		{ID: "skipped", Status: 429, UpstreamSkipped: true,
			ServerToolCalls: map[string]int64{"exa_search": 1}, ServerToolCallsReported: true},
	}
	start := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for i, row := range rows {
		row.Time = start.Add(time.Duration(i) * time.Second)
		row.UpstreamAttempts = observabilityAttempts(1)
		s.appendLog(row)
	}
	statsBefore := observabilityStatistics(t, s, nil)
	filtered := networkLogs(t, s, url.Values{"network": {"used"}, "limit": {"1"}, "offset": {"1"}})
	if filtered.Total != 2 || filtered.Offset != 1 || len(filtered.Items) != 1 || filtered.Items[0].ID != "used-first" {
		t.Fatalf("filter was not applied before pagination: %+v", filtered)
	}
	if filtered.Summary.Requests != 2 || filtered.Summary.PromptTokens != 30 || filtered.Summary.CompletionTokens != 11 || filtered.Summary.CachedTokens != 12 {
		t.Fatalf("filtered summary must count model requests rather than tool calls or one page: %+v", filtered.Summary)
	}
	for _, tc := range []struct {
		query url.Values
		id    string
	}{
		{url.Values{"network": {"used"}, "status": {"error"}}, "used-second"},
		{url.Values{"network": {"used"}, "provider": {"first"}}, "used-first"},
	} {
		view := networkLogs(t, s, tc.query)
		if view.Total != 1 || len(view.Items) != 1 || view.Items[0].ID != tc.id || view.Summary.Requests != 1 {
			t.Fatalf("network filter did not compose with existing filters: %+v", view)
		}
	}
	outOfRange := networkLogs(t, s, url.Values{"network": {"used"}, "limit": {"1"}, "offset": {"99"}})
	if outOfRange.Offset != 1 || len(outOfRange.Items) != 1 || outOfRange.Items[0].ID != "used-first" {
		t.Fatalf("filtered offset fallback changed: %+v", outOfRange)
	}
	for _, query := range []url.Values{nil, {"network": {"all"}}} {
		all := networkLogs(t, s, query)
		if all.Total != len(rows) || all.Summary.Requests != int64(len(rows)) {
			t.Fatalf("unfiltered request counts changed: %+v", all)
		}
	}
	statsAfter := observabilityStatistics(t, s, url.Values{"network": {"used"}})
	if !reflect.DeepEqual(statsAfter, statsBefore) || statsAfter.Requests != int64(len(rows)) || statsAfter.UpstreamRequests != 6 {
		t.Fatalf("log filtering changed independent model-request statistics: before=%+v after=%+v", statsBefore, statsAfter)
	}
}

func TestNetworkLogFieldsSurviveJournalAndSnapshotReload(t *testing.T) {
	dir := t.TempDir()
	// This is an old-format row with no server tool fields at either level.
	legacy := []byte(`[{"id":"legacy","time":"2026-10-23T11:00:00Z","status":200,"attempts":[{"status":200}]}]`)
	if err := os.WriteFile(filepath.Join(dir, "requests.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	s := observabilityService(t, dir)
	s.appendLog(LogEntry{ID: "network", Status: 200, PromptTokens: 10, CompletionTokens: 2,
		ServerTools:     []string{"vercel:exa_search", "vercel:tako_search"},
		ServerToolCalls: map[string]int64{"exa_search": 4, "tako_search": 0}, ServerToolCallsReported: true,
		UpstreamAttempts: observabilityAttempts(2), Attempts: []Attempt{
			{Status: 502, ServerTools: []string{"vercel:exa_search"},
				ServerToolCalls: map[string]int64{"exa_search": 1}, ServerToolCallsReported: true},
			{Status: 200, ServerTools: []string{"vercel:exa_search", "vercel:tako_search"},
				ServerToolCalls: map[string]int64{"exa_search": 3, "tako_search": 0}, ServerToolCallsReported: true},
		}})
	s.appendLog(LogEntry{ID: "reported-zero", Status: 200, ServerTools: []string{"vercel:exa_search"},
		ServerToolCalls: map[string]int64{"exa_search": 0}, ServerToolCallsReported: true,
		UpstreamAttempts: observabilityAttempts(1), Attempts: []Attempt{{Status: 200,
			ServerToolCalls: map[string]int64{"exa_search": 0}, ServerToolCallsReported: true}}})
	s.appendLog(LogEntry{ID: "not-reported", Status: 200, ServerTools: []string{"vercel:exa_search"},
		UpstreamAttempts: observabilityAttempts(1), Attempts: []Attempt{{Status: 200}}})
	before := networkLogs(t, s, nil)
	statsBefore := observabilityStatistics(t, s, nil)
	observabilityShutdown(t, s)
	check := func(current *Service) {
		t.Helper()
		if got := networkLogs(t, current, nil); !reflect.DeepEqual(got, before) {
			t.Fatalf("persistent logs lost tool metadata, attempts, or legacy rows: got=%+v want=%+v", got, before)
		}
		filtered := networkLogs(t, current, url.Values{"network": {"used"}})
		if filtered.Total != 1 || len(filtered.Items) != 1 || filtered.Items[0].ID != "network" || filtered.Summary.Requests != 1 {
			t.Fatalf("reload confused zero/unreported/legacy rows with actual tool calls: %+v", filtered)
		}
		if stats := observabilityStatistics(t, current, nil); !reflect.DeepEqual(stats, statsBefore) || stats.Requests != 4 || stats.UpstreamRequests != 4 {
			t.Fatalf("tool-call persistence changed model-request statistics: %+v", stats)
		}
	}
	journalReload := observabilityService(t, dir)
	check(journalReload)
	journalReload.mu.Lock()
	err := journalReload.compactRequestLogsLocked()
	journalReload.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	observabilityShutdown(t, journalReload)
	snapshotReload := observabilityService(t, dir)
	check(snapshotReload)
	var snapshot struct {
		Version int `json:"version"`
	}
	data, err := os.ReadFile(filepath.Join(dir, requestSnapshotName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &snapshot); err != nil || snapshot.Version != 1 {
		t.Fatalf("snapshot format version changed: version=%d error=%v", snapshot.Version, err)
	}
}
