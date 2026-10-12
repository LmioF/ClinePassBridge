package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// EgressConfig is independent of Config. GET responses always redact proxy
// userinfo; only the private egress.json contains authentication material.
type EgressConfig struct {
	Enabled            bool             `json:"enabled"`
	WaitTimeoutSeconds int              `json:"wait_timeout_seconds"`
	IPCooldownSeconds  int              `json:"ip_cooldown_seconds"`
	Endpoints          []EgressEndpoint `json:"endpoints"`
}

type EgressEndpoint struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Enabled     bool   `json:"enabled"`
	Group       string `json:"group"`
	ProxyURL    string `json:"proxy_url"`
	HasAuth     bool   `json:"has_auth,omitempty"`
	RPM         int    `json:"rpm"`
	MaxInflight int    `json:"max_inflight"`
}

func defaultEgressConfig() EgressConfig {
	return EgressConfig{WaitTimeoutSeconds: 5, IPCooldownSeconds: 60, Endpoints: []EgressEndpoint{}}
}

func normalizeEgressProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "direct" {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fail(400, "proxy_url must be direct or an HTTP/HTTPS/SOCKS5 proxy URL without a path or query")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fail(400, "unsupported egress proxy protocol")
	}
	if u.User != nil && (u.User.Username() == "***" || strings.ContainsAny(u.User.Username(), "\r\n")) {
		return "", fail(400, "a new proxy URL requires its original authentication values")
	}
	u.Path, u.RawPath = "", ""
	return u.String(), nil
}

func maskedEgressProxy(raw string) (string, bool) {
	if raw == "direct" {
		return raw, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	auth := u.User != nil
	if auth {
		u.User = url.UserPassword("***", "***")
	}
	return u.String(), auth
}

func publicEgressConfig(cfg EgressConfig) EgressConfig {
	cfg.Endpoints = append([]EgressEndpoint{}, cfg.Endpoints...)
	for i := range cfg.Endpoints {
		cfg.Endpoints[i].ProxyURL, cfg.Endpoints[i].HasAuth = maskedEgressProxy(cfg.Endpoints[i].ProxyURL)
	}
	return cfg
}

func parseEgressConfig(body []byte, old EgressConfig, preserve bool) (EgressConfig, error) {
	cfg := defaultEgressConfig()
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, fail(400, "invalid egress configuration JSON")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return cfg, fail(400, "invalid egress configuration JSON")
	}
	if cfg.WaitTimeoutSeconds < 0 || cfg.WaitTimeoutSeconds > 60 || cfg.IPCooldownSeconds < 1 || cfg.IPCooldownSeconds > 86400 || len(cfg.Endpoints) > 256 {
		return cfg, fail(400, "egress wait must be 0-60s, IP cooldown 1-86400s, and endpoints at most 256")
	}
	previous := make(map[string]EgressEndpoint, len(old.Endpoints))
	for _, endpoint := range old.Endpoints {
		previous[endpoint.ID] = endpoint
	}
	ids, limits := map[string]bool{}, map[string][2]int{}
	for i := range cfg.Endpoints {
		e := &cfg.Endpoints[i]
		e.ID, e.Group, e.Label = strings.TrimSpace(e.ID), strings.TrimSpace(e.Group), strings.TrimSpace(e.Label)
		if e.ID == "" || len(e.ID) > 64 || strings.ContainsAny(e.ID, "\r\n") || ids[e.ID] || len(e.Label) > 300 || len(e.Group) > 128 || strings.ContainsAny(e.Group, "\r\n") {
			return cfg, fail(400, "egress IDs must be unique and nonempty; labels and groups must be bounded")
		}
		ids[e.ID] = true
		if e.Group == "" {
			e.Group = e.ID
		}
		if e.RPM == 0 {
			e.RPM = 60
		}
		if e.MaxInflight == 0 {
			e.MaxInflight = 4
		}
		if e.RPM < 1 || e.RPM > 100000 || e.MaxInflight < 1 || e.MaxInflight > 1024 {
			return cfg, fail(400, "egress RPM must be 1-100000 and max_inflight 1-1024")
		}
		if oldLimit, exists := limits[e.Group]; exists && oldLimit != [2]int{e.RPM, e.MaxInflight} {
			return cfg, fail(400, "endpoints sharing an egress group must have identical RPM and max_inflight")
		}
		limits[e.Group] = [2]int{e.RPM, e.MaxInflight}
		oldEndpoint, exists := previous[e.ID]
		masked, _ := maskedEgressProxy(oldEndpoint.ProxyURL)
		if preserve && exists && (strings.TrimSpace(e.ProxyURL) == "" || e.ProxyURL == masked) {
			e.ProxyURL = oldEndpoint.ProxyURL
		}
		var err error
		e.ProxyURL, err = normalizeEgressProxy(e.ProxyURL)
		if err != nil {
			return cfg, err
		}
		e.HasAuth = false // Read-only GET field, never an instruction to change secrets.
	}
	if cfg.Endpoints == nil {
		cfg.Endpoints = []EgressEndpoint{}
	}
	return cfg, nil
}

func (s *Service) getEgressManager() *egressManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.egress == nil {
		s.egress = newEgressManager()
	}
	return s.egress
}

func (s *Service) egressConfigResponse() (EgressConfig, error) {
	p := s.getEgressManager()
	p.mu.Lock()
	defer p.mu.Unlock()
	return publicEgressConfig(p.cfg), nil
}

func (s *Service) saveEgressConfig(body []byte) (EgressConfig, error) {
	p := s.getEgressManager()
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	old, closed := p.cfg, p.closed
	p.mu.Unlock()
	if closed {
		return EgressConfig{}, fail(503, "egress transport is closing")
	}
	cfg, err := parseEgressConfig(body, old, true)
	if err != nil {
		return EgressConfig{}, err
	}
	if err := atomicJSON(filepath.Join(s.config().DataDir, "egress.json"), cfg); err != nil {
		return EgressConfig{}, fail(500, "egress configuration persistence failed")
	}
	p.apply(cfg)
	return publicEgressConfig(cfg), nil
}

// Called by configure while s.mu is held, before the plugin admits requests.
func (s *Service) loadEgressLocked() error {
	if s.egress == nil {
		s.egress = newEgressManager()
	}
	cfg := defaultEgressConfig()
	body, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "egress.json"))
	if err == nil {
		cfg, err = parseEgressConfig(body, cfg, false)
	}
	if err != nil && !os.IsNotExist(err) {
		return fail(500, "egress configuration could not be loaded; transport remains unavailable")
	}
	s.egress.apply(cfg)
	return nil
}

func (s *Service) egressEnabled() bool {
	p := s.getEgressManager()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Enabled
}

// Internal payload fields never enter the legacy host callback. Management
// callers opt in to a local credential proxy even when the shared pool is off.
func (s *Service) egressCredentialPayload(payload map[string]any, c Credential, management bool) map[string]any {
	if !s.egressEnabled() && !(management && strings.TrimSpace(c.ProxyURL) != "") {
		return payload
	}
	copy := make(map[string]any, len(payload)+2)
	for k, v := range payload {
		copy[k] = v
	}
	copy["egress_proxy_url"] = c.ProxyURL
	copy["egress_management"] = management
	return copy
}

// SetEgressRequestContext is an optional integration API for hosts with access
// to the original request context. Stock JSON ABI callbacks do not expose it.
func (s *Service) SetEgressRequestContext(callbackID string, ctx context.Context) func() {
	return s.getEgressManager().bindContext(callbackID, ctx)
}

func (s *Service) CancelEgressRequest(callbackID string) {
	s.getEgressManager().cancelCallback(callbackID)
}
