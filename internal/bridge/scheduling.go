package bridge

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

func (s *Service) schedulingResponse() (any, error) {
	hostAuths, hostErr := s.schedulingHostAuths()
	host := map[string]any{"available": hostErr == nil}
	if hostErr != nil {
		host["message"] = "当前 CPA 未提供账号运行状态；插件额度保护和出口管理仍可使用。"
	}
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		creds = append(creds, c)
	}
	logs := append([]LogEntry(nil), s.logs...)
	models := append([]Model(nil), s.cfg.Models...)
	s.mu.RUnlock()
	sortCredentials(creds)
	accounts := []map[string]any{}
	summary := map[string]int64{"available": 0, "quota_blocked": 0, "local_rejected": 0, "upstream_429": 0}
	for _, entry := range logs {
		if entry.UpstreamSkipped {
			summary["local_rejected"] += logRequestCount(entry)
		} else if entry.Status == 429 {
			summary["upstream_429"] += logRequestCount(entry)
		}
	}
	for _, c := range creds {
		c = normalizeQuotaCredential(c)
		windows := s.rateLimits.schedulingWindows(c.ID)
		blocked := c.QuotaBlock != nil
		account := map[string]any{"id": c.ID, "label": c.Label, "enabled": !c.Disabled, "priority": priorityValue(c), "quota_blocked": blocked, "backoffs": windows}
		if c.QuotaBlock != nil {
			labels := map[string]string{"five_hour": "五小时", "weekly": "每周", "monthly": "每月"}
			reasons := []string{}
			var retry time.Time
			for _, limit := range c.QuotaBlock.Limits {
				label := labels[limit.Type]
				if label == "" {
					label = limit.Type
				}
				reasons = append(reasons, label)
				if limit.ResetsAt != nil && limit.ResetsAt.After(retry) {
					retry = *limit.ResetsAt
				}
			}
			account["quota_reason"] = strings.Join(reasons, "、") + "额度耗尽"
			if !retry.IsZero() {
				account["retry_at"] = retry
			}
		}
		account["persistence_error"] = s.quotaStatus(c.ID)["persistence_error"]
		runtime := s.schedulingAuthMatch(c, hostAuths)
		cooldowns := schedulingModelCooldowns(runtime, models, time.Now().UTC())
		account["model_cooldowns"] = cooldowns
		if runtime != nil {
			value := map[string]any{"disabled": runtime.Disabled, "unavailable": runtime.Unavailable, "status": runtime.Status, "message": runtime.Message, "model_states_available": runtime.ModelStatesAvailable, "credential_cooldown": runtime.CredentialCooldown, "credential_blocked": runtime.CredentialBlocked}
			if !runtime.NextRetryAfter.IsZero() {
				value["next_retry_after"] = runtime.NextRetryAfter
			}
			account["host"] = value
		}
		if blocked {
			summary["quota_blocked"]++
		} else if !c.Disabled && schedulingAccountAvailable(runtime, models, cooldowns, windows) {
			summary["available"]++
		}
		accounts = append(accounts, account)
	}
	return managementJSON(200, map[string]any{"observed_at": time.Now().UTC(), "quota_auto_pause": true, "accounts": accounts, "summary": summary, "host": host, "counts_scope": "retained_plugin_logs"})
}

// Expose only active model deadlines. Aggregate auth.Unavailable may describe a
// single model and must not be presented as a credential-wide cooldown.
func schedulingModelCooldowns(runtime *schedulingHostAuth, models []Model, now time.Time) []map[string]any {
	rows := []map[string]any{}
	if runtime == nil || !runtime.ModelStatesAvailable {
		return rows
	}
	keys := make([]string, 0, len(runtime.ModelStates))
	for key := range runtime.ModelStates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		state := runtime.ModelStates[key]
		if !state.Unavailable || !state.NextRetryAfter.After(now) {
			continue
		}
		name, upstream := strings.TrimPrefix(key, Provider+"/"), key
		for _, model := range models {
			if key == model.ID || key == Provider+"/"+model.ID {
				name, upstream = model.ID, model.UpstreamID
				break
			}
		}
		rows = append(rows, map[string]any{"state_key": key, "model": name, "upstream_model": upstream, "retry_at": state.NextRetryAfter, "message": state.Message})
	}
	return rows
}

func schedulingAccountAvailable(runtime *schedulingHostAuth, models []Model, cooldowns, windows []map[string]any) bool {
	if runtime != nil && (runtime.Disabled || runtime.CredentialCooldown || runtime.CredentialBlocked) {
		return false
	}
	if runtime == nil || !runtime.ModelStatesAvailable || len(models) == 0 {
		return len(windows) == 0 && (runtime == nil || !runtime.Unavailable)
	}
	for _, model := range models {
		blocked := false
		// Host penalties use the client model key; sharing an upstream does not
		// make independently configured aliases share the host's penalty.
		for _, row := range cooldowns {
			key := str(row["state_key"])
			if key == "" {
				key = str(row["model"])
			}
			if key == model.ID || key == Provider+"/"+model.ID {
				blocked = true
				break
			}
		}
		// Plugin team-rate windows are acquired using the upstream model key.
		for _, row := range windows {
			key := str(row["model"])
			if key == model.ID || key == model.UpstreamID || key == Provider+"/"+model.ID {
				blocked = true
				break
			}
		}
		if !blocked {
			return true
		}
	}
	return false
}

// Join public (redacted) configuration with group budgets for the console.
func (s *Service) egressManagementResponse() (any, error) {
	cfg, err := s.egressConfigResponse()
	if err != nil {
		return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
	}
	state := s.egressStatus()
	groups := map[string]map[string]any{}
	if rows, ok := state["groups"].([]map[string]any); ok {
		for _, row := range rows {
			groups[str(row["group"])] = row
		}
	}
	rows := []map[string]any{}
	for _, endpoint := range cfg.Endpoints {
		row, _ := decodeObject(jsonBytes(endpoint))
		group := groups[endpoint.Group]
		row["status"] = map[string]any{"requests_last_minute": group["used_last_60s"], "inflight": group["inflight"], "retry_at": group["cooldown_until"]}
		rows = append(rows, row)
	}
	return managementJSON(200, map[string]any{"enabled": cfg.Enabled, "wait_timeout_seconds": cfg.WaitTimeoutSeconds, "ip_cooldown_seconds": cfg.IPCooldownSeconds, "endpoints": rows})
}

func (s *Service) resetScheduling(r ManagementRequest) (any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(r.Body, &in) != nil || in.ID == "" {
		return managementJSON(400, map[string]any{"error": "请选择要清除冷却的账号"})
	}
	s.mu.RLock()
	c, ok := s.creds[in.ID]
	s.mu.RUnlock()
	if !ok {
		return managementJSON(404, map[string]any{"error": "账号不存在"})
	}
	if c.Disabled || c.QuotaBlock != nil {
		return managementJSON(409, map[string]any{"error": "账号已停用或额度耗尽，请先复查用量与账号状态"})
	}
	s.rateLimits.resetSchedulingWindows(c.ID)
	entries, err := s.schedulingHostAuths()
	if err == nil {
		if entry := s.schedulingAuthMatch(c, entries); entry != nil && entry.AuthIndex != "" {
			err = s.call("host.routing.reset_cooldown", map[string]any{"auth_index": entry.AuthIndex}, nil)
			if err == nil {
				return managementJSON(200, map[string]any{"plugin_reset": true, "host_reset": true})
			}
		}
	}
	return managementJSON(200, map[string]any{"plugin_reset": true, "host_reset": false, "warning": "插件退避已清除；当前宿主未完成 CPA 冷却重置。"})
}
