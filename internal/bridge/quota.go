package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

const quotaBlockMarker = "[clinepassbridge:quota_block]"

// Identity is verified by the authenticated plan API, never inferred from labels,
// a proxy, an error message, or an identical percentage. No upstream IDs or keys
// are stored here. The key hash prevents an old account surviving key rotation.
type QuotaIdentity struct {
	AccountHash string `json:"account_hash"`
	KeyHash     string `json:"key_hash"`
}

type QuotaBlockLimit struct {
	Type     string     `json:"type"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// QuotaBlock is independent from Credential.Disabled, which always retains the
// operator's choice. Expiry permits another usage check; it does not enable auth.
type QuotaBlock struct {
	AccountHash string            `json:"account_hash,omitempty"`
	KeyHash     string            `json:"key_hash"`
	Source      string            `json:"source"`
	Reason      string            `json:"reason"`
	RetryAt     *time.Time        `json:"retry_at,omitempty"`
	BlockedAt   time.Time         `json:"blocked_at"`
	ObservedAt  time.Time         `json:"observed_at"`
	Limits      []QuotaBlockLimit `json:"limits"`
}

func quotaAutoPauseEnabled(cfg Config) bool {
	return cfg.QuotaAutoPause == nil || *cfg.QuotaAutoPause
}

func quotaKeyHash(c Credential) string { return estimateHash("quota-key", c.APIKey) }

func normalizeQuotaCredential(c Credential) Credential {
	key := quotaKeyHash(c)
	if c.QuotaIdentity != nil && (key == "" || c.QuotaIdentity.KeyHash != key || c.QuotaIdentity.AccountHash == "") {
		c.QuotaIdentity = nil
	}
	if c.QuotaBlock != nil && (key == "" || c.QuotaBlock.KeyHash != key || len(c.QuotaBlock.Limits) == 0) {
		c.QuotaBlock = nil
	}
	return c
}

func quotaStateEqual(a, b Credential) bool {
	return bytes.Equal(jsonBytes(a.QuotaBlock), jsonBytes(b.QuotaBlock)) &&
		bytes.Equal(jsonBytes(a.QuotaIdentity), jsonBytes(b.QuotaIdentity))
}

// Caller holds mu. A persisted verified identity can keep sampling and grouping
// working after a reload; unknown credentials remain distinct.
func (s *Service) quotaAccountHashLocked(c Credential) string {
	c = normalizeQuotaCredential(c)
	if c.QuotaIdentity != nil {
		return c.QuotaIdentity.AccountHash
	}
	if entry := s.usageCache[c.ID]; entry != nil && entry.fingerprint == usageFingerprint(c) {
		return entry.value.accountHash
	}
	return ""
}

func (s *Service) quotaStatus(credentialID string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := normalizeQuotaCredential(s.creds[credentialID])
	paused := quotaAutoPauseEnabled(s.cfg) && c.QuotaBlock != nil
	// Copy via JSON so callers never receive mutable live state.
	var block map[string]any
	if c.QuotaBlock != nil {
		_ = json.Unmarshal(jsonBytes(c.QuotaBlock), &block)
		for _, field := range []string{"key_hash", "account_hash", "reason", "retry_at"} {
			delete(block, field)
		}
	}
	return map[string]any{
		"quota_auto_pause": quotaAutoPauseEnabled(s.cfg), "manual_disabled": c.Disabled,
		"quota_paused": paused, "effective_disabled": c.Disabled || paused,
		"quota_block": block, "persistence_error": s.quotaWriteErrors[credentialID],
	}
}

func (s *Service) quotaPersistenceError(next Credential, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.creds[next.ID]
	if !exists || current.APIKey != next.APIKey || current.QuotaRevision != next.QuotaRevision {
		return // A late write failure must not contaminate a replaced key.
	}
	if s.quotaWriteErrors == nil {
		s.quotaWriteErrors = map[string]string{}
	}
	if err == nil {
		delete(s.quotaWriteErrors, next.ID)
	} else {
		s.quotaWriteErrors[next.ID] = safeError(err)
	}
}

// Only the quota-owned fields change. In particular, the stored "disabled" is
// never the effective runtime flag. Let the watcher reparse the physical file:
// host.auth.save would also create a generic registration for rotated auths.
func (s *Service) persistQuotaCredential(next Credential) (err error) {
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	s.mu.Lock()
	current, exists := s.creds[next.ID]
	if !exists || current.APIKey != next.APIKey {
		s.mu.Unlock()
		return fail(409, "quota credential changed before persistence")
	}
	next.QuotaRevision = id()
	current.QuotaRevision = next.QuotaRevision
	if next.QuotaBlock != nil {
		// Even direct callers must fail closed before touching the filesystem.
		current.QuotaBlock, current.QuotaIdentity = next.QuotaBlock, next.QuotaIdentity
	}
	s.creds[next.ID] = current
	s.mu.Unlock()
	defer func() { s.quotaPersistenceError(next, err) }()
	file, err := s.affinityFile(current)
	if err != nil {
		return err
	}
	for _, key := range []string{"quota_block", "quota_identity", "quota_revision"} {
		delete(file.updated, key)
	}
	if next.QuotaBlock != nil {
		file.updated["quota_block"] = jsonBytes(next.QuotaBlock)
	}
	if next.QuotaIdentity != nil {
		file.updated["quota_identity"] = jsonBytes(next.QuotaIdentity)
	}
	file.updated["quota_revision"] = jsonBytes(next.QuotaRevision)
	// Preserve an external edit rather than replacing its fields with a stale copy.
	latest, err := os.ReadFile(file.path)
	if err != nil || !bytes.Equal(latest, file.original) {
		return fail(409, "quota credential file changed before persistence")
	}
	if err = atomicJSON(file.path, file.updated); err != nil {
		return fmt.Errorf("persist quota state: %w", err)
	}
	s.mu.Lock()
	if c, ok := s.creds[next.ID]; ok && c.APIKey == next.APIKey && c.QuotaRevision == next.QuotaRevision {
		c.QuotaBlock, c.QuotaIdentity, c.QuotaRevision = next.QuotaBlock, next.QuotaIdentity, next.QuotaRevision
		s.creds[next.ID] = c
	}
	s.mu.Unlock()
	return nil
}

// Call after saving Config.QuotaAutoPause. Rewriting a quota revision makes the
// watcher apply a changed effective Disabled even when the block itself is equal.
func (s *Service) reconcileQuotaScheduling() error {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.RLock()
	disabled := !quotaAutoPauseEnabled(s.cfg)
	changed := disabled != s.quotaAppliedDisabled
	var creds []Credential
	for _, c := range s.creds {
		if c.QuotaBlock != nil && changed || s.quotaWriteErrors[c.ID] != "" {
			creds = append(creds, normalizeQuotaCredential(c))
		}
	}
	s.mu.RUnlock()
	var errs []error
	for _, c := range creds {
		if err := s.persistQuotaCredential(c); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		s.mu.Lock()
		s.quotaAppliedDisabled = disabled
		s.mu.Unlock()
	}
	return errors.Join(errs...)
}

func canonicalQuotaType(value string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(value), "-", "_"), " ", "_")) {
	case "weekly", "week":
		return "weekly"
	case "five_hour", "five_hours", "5_hour", "5_hours":
		return "five_hour"
	case "monthly", "month":
		return "monthly"
	case "quota":
		return "quota"
	}
	return ""
}

func mergeQuotaBlocks(c Credential, blocks ...*QuotaBlock) *QuotaBlock {
	var result *QuotaBlock
	limits := map[string]QuotaBlockLimit{}
	for _, block := range blocks {
		if block == nil {
			continue
		}
		if result == nil {
			copy := *block
			result = &copy
		}
		if block.BlockedAt.Before(result.BlockedAt) {
			result.BlockedAt = block.BlockedAt
		}
		if block.ObservedAt.After(result.ObservedAt) {
			result.ObservedAt, result.Source = block.ObservedAt, block.Source
		}
		for _, limit := range block.Limits {
			old, exists := limits[limit.Type]
			if !exists || old.ResetsAt == nil || limit.ResetsAt != nil && limit.ResetsAt.After(*old.ResetsAt) {
				limits[limit.Type] = limit
			}
		}
	}
	if result == nil {
		return nil
	}
	result.KeyHash = quotaKeyHash(c)
	result.Reason = "quota_exhausted"
	result.RetryAt = nil
	if c.QuotaIdentity != nil {
		result.AccountHash = c.QuotaIdentity.AccountHash
	}
	result.Limits = nil
	for _, limit := range limits {
		result.Limits = append(result.Limits, limit)
		if limit.ResetsAt != nil && (result.RetryAt == nil || limit.ResetsAt.After(*result.RetryAt)) {
			value := *limit.ResetsAt
			result.RetryAt = &value
		}
	}
	sort.Slice(result.Limits, func(i, j int) bool { return result.Limits[i].Type < result.Limits[j].Type })
	return result
}

// Caller holds mu. Only source and independently verified sibling keys join.
func (s *Service) quotaGroupLocked(source Credential) []Credential {
	account := s.quotaAccountHashLocked(source)
	var group []Credential
	for _, c := range s.creds {
		if s.revoked[c.ID] || c.ID != source.ID && (account == "" || s.quotaAccountHashLocked(c) != account) {
			continue
		}
		c = normalizeQuotaCredential(c)
		if account != "" {
			c.QuotaIdentity = &QuotaIdentity{account, quotaKeyHash(c)}
		}
		group = append(group, c)
	}
	sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
	return group
}

func (s *Service) saveQuotaGroup(group []Credential) error {
	// Publish every sibling's block together before the first potentially slow IO.
	var writes []Credential
	s.mu.Lock()
	for _, next := range group {
		current, exists := s.creds[next.ID]
		if !exists || current.APIKey != next.APIKey {
			continue
		}
		changed := !quotaStateEqual(current, next)
		if changed || s.quotaWriteErrors[next.ID] != "" {
			writes = append(writes, next)
		}
		if changed && next.QuotaBlock != nil {
			current.QuotaBlock, current.QuotaIdentity = next.QuotaBlock, next.QuotaIdentity
			current.QuotaRevision = id()
			s.creds[next.ID] = current
		}
	}
	s.mu.Unlock()
	var errs []error
	for _, next := range writes {
		if err := s.persistQuotaCredential(next); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Main executor integrates this hook on an upstream error. No error or usage
// text alone may merge unknown keys into an account, and stale rotated keys are ignored.
func (s *Service) observeQuotaError(source Credential, err error) error {
	var upstream *upstreamError
	if !errors.As(err, &upstream) || upstream.Local || upstream.Kind != "upstream_quota_exhausted" {
		return nil
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.RLock()
	current, exists := s.creds[source.ID]
	if !exists || current.APIKey != source.APIKey || s.revoked[source.ID] {
		s.mu.RUnlock()
		return nil
	}
	group := s.quotaGroupLocked(current)
	s.mu.RUnlock()
	now := time.Now().UTC()
	typeName := upstream.QuotaType
	if typeName == "" {
		typeName = "quota"
	}
	newBlock := &QuotaBlock{Source: "error", BlockedAt: now, ObservedAt: now, Limits: []QuotaBlockLimit{{Type: typeName, ResetsAt: upstream.QuotaResetAt}}}
	for i := range group {
		group[i].QuotaBlock = mergeQuotaBlocks(group[i], group[i].QuotaBlock, newBlock)
	}
	s.wakeUsageSampler()
	return s.saveQuotaGroup(group)
}

func quotaUsageHasRoom(block *QuotaBlock, value credentialUsage, started time.Time) bool {
	if block == nil || value.Status != "ok" || value.PlanError != "" || value.UpdatedAt == nil || !started.After(block.ObservedAt) {
		return false
	}
	known := map[string]usageLimit{}
	for _, limit := range value.Limits {
		if kind := canonicalQuotaType(limit.Type); kind != "" {
			if limit.PercentUsed == nil || *limit.PercentUsed < 0 || math.IsNaN(*limit.PercentUsed) || math.IsInf(*limit.PercentUsed, 0) {
				return false
			}
			known[kind] = limit
			if *limit.PercentUsed >= 100 {
				return false
			}
		}
	}
	for _, blocked := range block.Limits {
		if blocked.ResetsAt != nil && started.Before(*blocked.ResetsAt) {
			return false
		}
		if blocked.Type == "quota" {
			if len(known) == 0 {
				return false
			}
		} else if _, present := known[blocked.Type]; !present {
			return false
		}
	}
	return true
}

// Runs only after an actual usage response, outside mu. Queries keep running
// for automatically paused keys because the manual Disabled flag stays false.
func (s *Service) observeQuotaUsage(source Credential, value credentialUsage, started time.Time) error {
	if value.Status != "ok" || value.UpdatedAt == nil {
		return nil
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.Lock()
	current, exists := s.creds[source.ID]
	if !exists || s.revoked[source.ID] || usageFingerprint(current) != usageFingerprint(source) {
		s.mu.Unlock()
		return nil
	}
	if value.accountHash != "" {
		current.QuotaIdentity = &QuotaIdentity{value.accountHash, quotaKeyHash(current)}
		// Include the new identity in grouping, but don't mark it durable yet.
		if entry := s.usageCache[source.ID]; entry != nil {
			entry.value.accountHash = value.accountHash
		}
	}
	group := s.quotaGroupLocked(current)
	s.mu.Unlock()
	var blocks []*QuotaBlock
	for _, c := range group {
		blocks = append(blocks, c.QuotaBlock)
	}
	exhausted := &QuotaBlock{Source: "usage", BlockedAt: *value.UpdatedAt, ObservedAt: *value.UpdatedAt}
	for _, limit := range value.Limits {
		kind := canonicalQuotaType(limit.Type)
		if kind != "" && kind != "quota" && limit.PercentUsed != nil && *limit.PercentUsed >= 100 {
			exhausted.Limits = append(exhausted.Limits, QuotaBlockLimit{kind, limit.ResetsAt})
		}
	}
	if len(exhausted.Limits) > 0 {
		blocks = append(blocks, exhausted)
	}
	for i := range group {
		block := mergeQuotaBlocks(group[i], blocks...)
		if len(exhausted.Limits) == 0 && quotaUsageHasRoom(block, value, started) {
			block = nil
		}
		group[i].QuotaBlock = block
	}
	return s.saveQuotaGroup(group)
}

// A reset must trigger a fresh fetch even if the old cached sample's TTL remains.
func quotaRecoveryCheckDue(c Credential, value credentialUsage, now time.Time) bool {
	c = normalizeQuotaCredential(c)
	if c.QuotaBlock == nil {
		return false
	}
	for _, limit := range c.QuotaBlock.Limits {
		if limit.ResetsAt != nil && !now.Before(*limit.ResetsAt) && (value.CheckedAt == nil || value.CheckedAt.Before(*limit.ResetsAt)) {
			return true
		}
	}
	return false
}
