package bridge

import (
	"strings"
	"time"
)

// Timings and shape labels only; never persist stream content or tool arguments.
type StreamDiagnostics struct {
	IdleTimeoutSeconds  float64    `json:"idle_timeout_seconds"`
	IdleWaitMS          int64      `json:"idle_wait_ms"`
	UpstreamReadCount   int        `json:"upstream_read_count"`
	LastUpstreamByteAt  *time.Time `json:"last_upstream_byte_at,omitempty"`
	LastFrameAt         *time.Time `json:"last_frame_at,omitempty"`
	LastFrameKind       string     `json:"last_frame_kind,omitempty"`
	FirstEffectiveKind  string     `json:"first_effective_kind,omitempty"`
	LastEffectiveKind   string     `json:"last_effective_kind,omitempty"`
	DownstreamEmitCount int        `json:"downstream_emit_count"`
	DownstreamEmitMS    int64      `json:"downstream_emit_ms"`
	DownstreamEmitMaxMS int64      `json:"downstream_emit_max_ms"`
}

func (d *StreamDiagnostics) observeFrame(kind string) {
	at := time.Now().UTC()
	d.LastFrameAt, d.LastFrameKind = &at, kind
	switch kind {
	case "text", "whitespace", "reasoning", "tool", "refusal":
		if d.FirstEffectiveKind == "" {
			d.FirstEffectiveKind = kind
		}
		d.LastEffectiveKind = kind
	}
}

func streamFrameKind(j map[string]any) string {
	whitespace, finished := false, false
	for _, raw := range list(j["choices"]) {
		ch := object(raw)
		delta := object(ch["delta"])
		if str(delta["reasoning_content"]) != "" || str(delta["reasoning"]) != "" {
			return "reasoning"
		}
		if effectiveFunctionDelta(object(delta["function_call"])) {
			return "tool"
		}
		for _, tool := range list(delta["tool_calls"]) {
			if effectiveFunctionDelta(object(object(tool)["function"])) {
				return "tool"
			}
		}
		if str(delta["refusal"]) != "" {
			return "refusal"
		}
		if text := str(delta["content"]); text != "" {
			if strings.TrimSpace(text) != "" {
				return "text"
			}
			whitespace = true
		}
		finished = finished || str(ch["finish_reason"]) != ""
	}
	if whitespace {
		return "whitespace"
	}
	if finished {
		return "finish"
	}
	return "metadata"
}
