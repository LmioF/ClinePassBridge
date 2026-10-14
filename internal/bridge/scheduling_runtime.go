package bridge

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Deliberately decode only routing metadata. host.auth.list may include paths,
// emails and identifiers that the scheduling console does not need.
type schedulingHostAuth struct {
	ID                   string                          `json:"id"`
	Name                 string                          `json:"name"`
	AuthIndex            string                          `json:"auth_index"`
	Provider             string                          `json:"provider"`
	Type                 string                          `json:"type"`
	Disabled             bool                            `json:"disabled"`
	Unavailable          bool                            `json:"unavailable"`
	Status               string                          `json:"status"`
	Message              string                          `json:"status_message"`
	NextRetryAfter       time.Time                       `json:"next_retry_after"`
	ModelStatesAvailable bool                            `json:"model_states_available"`
	CredentialCooldown   bool                            `json:"credential_cooldown"`
	CredentialBlocked    bool                            `json:"credential_blocked"`
	ModelStates          map[string]schedulingModelState `json:"model_states"`
}

type schedulingModelState struct {
	Status         string    `json:"status"`
	Message        string    `json:"status_message"`
	Unavailable    bool      `json:"unavailable"`
	NextRetryAfter time.Time `json:"next_retry_after"`
}

func (s *Service) schedulingHostAuths() ([]schedulingHostAuth, error) {
	var response struct {
		Files []schedulingHostAuth `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{}, &response); err != nil {
		return nil, err
	}
	result := []schedulingHostAuth{}
	for _, entry := range response.Files {
		if entry.Provider == Provider || entry.Type == Provider {
			entry.Message = safeError(errors.New(entry.Message))
			for model, state := range entry.ModelStates {
				state.Message = safeError(errors.New(state.Message))
				entry.ModelStates[model] = state
			}
			result = append(result, entry)
		}
	}
	return result, nil
}

func (s *Service) schedulingAuthMatch(c Credential, entries []schedulingHostAuth) *schedulingHostAuth {
	s.mu.RLock()
	filename := s.authFiles[c.ID]
	s.mu.RUnlock()
	for i := range entries {
		entry := &entries[i]
		if entry.ID == routingID(c, filename) || (filename != "" && entry.Name == filename) {
			return entry
		}
	}
	return nil
}

func (l *rateLimiter) schedulingWindows(credentialID string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	windows := []map[string]any{}
	for key, window := range l.windows {
		if key.credential == credentialID && (l.now().Before(window.retryAt) || window.probe != nil) {
			windows = append(windows, map[string]any{"model": key.model, "retry_at": window.retryAt, "scope": window.scope, "probing": window.probe != nil})
		}
	}
	return windows
}

func (l *rateLimiter) resetSchedulingWindows(credentialID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, window := range l.windows {
		if key.credential == credentialID {
			// Do not admit a second recovery probe while an existing one runs.
			if window.probe != nil {
				window.retryAt = l.now()
				continue
			}
			delete(l.windows, key)
		}
	}
}

func (s *Service) schedulingSetCredential(r ManagementRequest) (any, error) {
	var in struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if json.Unmarshal(r.Body, &in) != nil || strings.TrimSpace(in.ID) == "" || in.Enabled == nil {
		return managementJSON(400, map[string]any{"error": "请选择账号并指定启停状态"})
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.RLock()
	c, exists := s.creds[in.ID]
	s.mu.RUnlock()
	if !exists {
		return managementJSON(404, map[string]any{"error": "账号不存在"})
	}
	baseline := c
	c.Disabled = !*in.Enabled
	if err := s.saveCredential(c, baseline); err != nil {
		return managementJSON(409, map[string]any{"error": safeError(err)})
	}
	s.wakeUsageSampler()
	return managementJSON(200, map[string]any{"id": c.ID, "enabled": !c.Disabled})
}
