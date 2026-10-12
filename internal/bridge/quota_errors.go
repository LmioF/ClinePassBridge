package bridge

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var clineQuotaPattern = regexp.MustCompile(`(?i)\b(weekly|monthly|five[_ -]?hours?|5[_ -]?hours?)\s+cline\s*pass\s+limit\b`)
var quotaResetPattern = regexp.MustCompile(`(?i)\bresets?\s+in\s+((?:\d+(?:\.\d+)?\s*(?:days?|hours?|minutes?|seconds?|d|h|m|s)\s*)+)`)
var quotaDurationPattern = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(days?|hours?|minutes?|seconds?|d|h|m|s)`)

func quotaErrorType(code, message string) string {
	if match := clineQuotaPattern.FindStringSubmatch(message); len(match) == 2 {
		// A named limit being mentioned is not evidence of exhaustion.
		lower := strings.ToLower(message)
		if strings.Contains(lower, "reached") || strings.Contains(lower, "exceeded") || strings.Contains(lower, "exhausted") {
			kind := strings.ToLower(match[1])
			if strings.HasPrefix(kind, "five") || strings.HasPrefix(kind, "5") {
				return "five_hour"
			}
			return kind
		}
	}
	lower := strings.ToLower(message)
	code = strings.ToLower(code)
	if strings.Contains(code, "insufficient_quota") || strings.Contains(code, "quota_exceeded") ||
		strings.Contains(lower, "quota exhausted") || strings.Contains(lower, "quota exceeded") ||
		strings.Contains(lower, "insufficient credits") || strings.Contains(lower, "insufficient balance") {
		return "quota"
	}
	return ""
}

// Subscription resets can legitimately exceed a day. This parser is deliberately
// separate from the short team-TPM retry policy, which another module owns.
func quotaResetTime(headers http.Header, message string, now time.Time) *time.Time {
	if raw := headers.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 && seconds <= 366*86400 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
			value := now.Add(time.Duration(math.Ceil(seconds)) * time.Second)
			return &value
		}
		if value, err := http.ParseTime(raw); err == nil && value.After(now) && value.Sub(now) <= 366*24*time.Hour {
			return &value
		}
	}
	match := quotaResetPattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return nil
	}
	var seconds float64
	for _, part := range quotaDurationPattern.FindAllStringSubmatch(match[1], -1) {
		amount, err := strconv.ParseFloat(part[1], 64)
		if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return nil
		}
		switch strings.ToLower(part[2])[0] {
		case 'd':
			seconds += amount * 86400
		case 'h':
			seconds += amount * 3600
		case 'm':
			seconds += amount * 60
		case 's':
			seconds += amount
		}
	}
	if seconds <= 0 || seconds > 366*86400 {
		return nil
	}
	value := now.Add(time.Duration(math.Ceil(seconds)) * time.Second)
	return &value
}
