package bridge

// Tool declarations do not prove execution. Only positive upstream-reported
// counts qualify, and a locally skipped request never used an upstream tool.
func hasServerToolCalls(entry LogEntry) bool {
	if entry.UpstreamSkipped {
		return false
	}
	for _, count := range entry.ServerToolCalls {
		if count > 0 {
			return true
		}
	}
	return false
}
