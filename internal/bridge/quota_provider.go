package bridge

import (
	"encoding/json"
	"math"
	"net/url"
	"time"
)

// These fields follow CPA's JSON ABI, independently of the plugin console's usage DTO.
type quotaResponse struct {
	Subscription *quotaSubscription `json:"subscription,omitempty"`
	Groups       []quotaGroup       `json:"groups"`
}

type quotaSubscription struct {
	Plan string `json:"plan"`
}

type quotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []quotaBucket `json:"buckets"`
}

type quotaBucket struct {
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime,omitempty"`
}

func (s *Service) fetchQuota(raw json.RawMessage) (quotaResponse, error) {
	var req struct {
		AuthID         string `json:"auth_id"`
		Provider       string `json:"provider"`
		HostCallbackID string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &req) != nil || req.Provider != Provider || req.AuthID == "" {
		return quotaResponse{}, fail(400, "Cline Pass quota requires a matching credential")
	}
	// CPA resolves auth_index to its routing identity. Never fall back to the first
	// account, and allow manually disabled or quota-paused accounts to query usage.
	credentialID := ""
	s.mu.RLock()
	for id, c := range s.creds {
		if !s.revoked[id] && routingID(c, s.authFiles[id]) == req.AuthID {
			credentialID = id
			break
		}
	}
	s.mu.RUnlock()
	if credentialID == "" {
		return quotaResponse{}, fail(404, "Cline Pass quota credential is no longer available")
	}
	value, err := s.credentialUsage(ManagementRequest{
		Query: url.Values{"id": {credentialID}, "refresh": {"1"}}, HostCallbackID: req.HostCallbackID,
	})
	if err != nil {
		return quotaResponse{}, err
	}
	if value.Status != "ok" {
		status := 502
		switch value.Status {
		case "unauthorized":
			status = 401
		case "rate_limited":
			status = 429
		case "timeout":
			status = 504
		}
		// The console can label stale values; CPA's normalized response cannot.
		return quotaResponse{}, fail(status, "Cline Pass quota query failed: "+value.Status)
	}
	group := quotaGroup{DisplayName: "Cline Pass", Buckets: []quotaBucket{}}
	for _, limit := range value.Limits {
		if limit.PercentUsed == nil || math.IsNaN(*limit.PercentUsed) || math.IsInf(*limit.PercentUsed, 0) || *limit.PercentUsed < 0 {
			continue
		}
		window := limit.Type
		switch canonicalQuotaType(window) {
		case "five_hour":
			window = "5h"
		case "weekly":
			window = "7d"
		case "monthly":
			window = "monthly"
		}
		bucket := quotaBucket{Window: window, RemainingFraction: 1 - min(*limit.PercentUsed, 100)/100}
		if limit.ResetsAt != nil {
			bucket.ResetTime = limit.ResetsAt.Format(time.RFC3339Nano)
		}
		group.Buckets = append(group.Buckets, bucket)
	}
	if len(group.Buckets) == 0 {
		return quotaResponse{}, fail(502, "Cline Pass returned no usable quota windows")
	}
	response := quotaResponse{Groups: []quotaGroup{group}}
	if value.PlanError == "" && value.Plan != nil && value.Plan.Name != "" {
		response.Subscription = &quotaSubscription{Plan: value.Plan.Name}
	}
	return response, nil
}
