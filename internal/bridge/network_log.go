package bridge

import (
	"encoding/json"
	"math"
	"strings"
)

// recordServerTools records only the known network tool types in the final
// outbound body. Call it when an upstream attempt starts, after local admission.
// Tool schemas, options and client function definitions do not belong in logs.
func recordServerTools(j map[string]any, entry *LogEntry, attempt *Attempt) {
	seen := make(map[string]bool)
	for _, kind := range entry.ServerTools {
		seen[kind] = true
	}
	attemptSeen := make(map[string]bool)
	for _, raw := range list(j["tools"]) {
		tool := object(raw)
		if builtinServerToolFunctionName(tool) == "" {
			continue
		}
		kind := strings.TrimSpace(str(tool["type"]))
		if !attemptSeen[kind] {
			attempt.ServerTools = append(attempt.ServerTools, kind)
			attemptSeen[kind] = true
		}
		if !seen[kind] {
			entry.ServerTools = append(entry.ServerTools, kind)
			seen[kind] = true
		}
	}
}

// observeServerToolCalls reads explicit gateway cumulative counters. A snapshot
// may appear at several roots or repeat in later SSE frames: keep each attempt's
// maximum and add only its increase to the parent request's total.
func observeServerToolCalls(j map[string]any, entry *LogEntry, attempt *Attempt) {
	if j == nil {
		return
	}
	roots := []map[string]any{j}
	if data := object(j["data"]); data != nil {
		roots = append(roots, data)
	}
	for _, root := range roots {
		for _, raw := range list(root["choices"]) {
			choice := object(raw)
			for _, key := range []string{"message", "delta"} {
				if value := object(choice[key]); value != nil {
					roots = append(roots, value)
				}
			}
		}
	}
	for _, root := range roots {
		for _, key := range []string{"provider_metadata", "providerMetadata"} {
			gateway := object(object(root[key])["gateway"])
			counts, valid := serverToolCallCounts(gateway["gatewayToolCalls"])
			if !valid {
				continue
			}
			attempt.ServerToolCallsReported = true
			entry.ServerToolCallsReported = true
			if attempt.ServerToolCalls == nil {
				attempt.ServerToolCalls = make(map[string]int64)
			}
			if entry.ServerToolCalls == nil {
				entry.ServerToolCalls = make(map[string]int64)
			}
			for name, count := range counts {
				previous, reported := attempt.ServerToolCalls[name]
				if !reported || count > previous {
					attempt.ServerToolCalls[name] = count
					entry.ServerToolCalls[name] += count - previous
				}
			}
		}
	}
}

func serverToolCallCounts(raw any) (map[string]int64, bool) {
	values := object(raw)
	if values == nil {
		return nil, false
	}
	counts := make(map[string]int64, len(values))
	for name, rawCount := range values {
		// These are the six built-ins whose names the bridge understands. Do not
		// infer names for future tools or persist arbitrary metadata keys.
		if builtinServerToolFunctionName(map[string]any{"type": "vercel:" + name}) != name || name == "" {
			continue
		}
		count, valid := serverToolCallCount(rawCount)
		if !valid {
			return nil, false
		}
		counts[name] = count
	}
	// Unknown-only objects do not establish a zero count for known tools.
	return counts, len(values) == 0 || len(counts) > 0
}

func serverToolCallCount(raw any) (int64, bool) {
	var count int64
	switch value := raw.(type) {
	case json.Number:
		var err error
		count, err = value.Int64()
		if err != nil {
			return 0, false
		}
	case int:
		count = int64(value)
	case int64:
		count = value
	case float64:
		if value < 0 || value >= float64(math.MaxInt64) || math.Trunc(value) != value {
			return 0, false
		}
		count = int64(value)
	default:
		return 0, false
	}
	return count, count >= 0
}
