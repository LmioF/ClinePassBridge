package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const errorRulesFile = "error-rules.json"

type errorRulesDocument struct {
	Rules []RequestErrorRule `json:"rules"`
}

type errorRuleSyncFailure struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Error string `json:"error"`
}

type errorRuleSyncResult struct {
	Total     int                    `json:"total"`
	Succeeded int                    `json:"succeeded"`
	Failed    []errorRuleSyncFailure `json:"failed"`
}

type errorRulesResponse struct {
	Rules    []RequestErrorRule  `json:"rules"`
	Defaults []RequestErrorRule  `json:"defaults"`
	Sync     errorRuleSyncResult `json:"sync"`
	Inherits bool                `json:"inherits_cpa"`
}

func cloneErrorRules(rules []RequestErrorRule) []RequestErrorRule {
	copy := append([]RequestErrorRule{}, rules...)
	for i := range copy {
		if copy[i].Match != nil {
			copy[i].Match = append([]string{}, copy[i].Match...)
		}
		if copy[i].MatchRegexr != nil {
			copy[i].MatchRegexr = append([]string{}, copy[i].MatchRegexr...)
		}
	}
	return copy
}

func (s *Service) currentErrorRules() []RequestErrorRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneErrorRules(s.errorRules)
}

// Reject ambiguous duplicate keys as well as unknown fields and trailing JSON.
// Never include supplied JSON, patterns or decoder errors in an API error.
func strictErrorRulesObject(raw []byte, out any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 1<<20 || raw[0] != '{' {
		return fail(400, "请求必须是 JSON 对象")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := checkErrorRulesJSONKeys(decoder); err != nil {
		return fail(400, "JSON 格式错误或包含重复字段")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fail(400, "JSON 格式错误或包含未知字段")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fail(400, "请求只接受一个 JSON 对象")
	}
	return nil
}

func checkErrorRulesJSONKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON key")
			}
			seen[name] = true
		}
		if err := checkErrorRulesJSONKeys(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func decodeErrorRules(raw []byte) ([]RequestErrorRule, error) {
	var document struct {
		Rules json.RawMessage `json:"rules"`
	}
	if err := strictErrorRulesObject(raw, &document); err != nil {
		return nil, err
	}
	array := bytes.TrimSpace(document.Rules)
	if len(array) == 0 || array[0] != '[' {
		return nil, fail(400, "rules 必须是数组；空数组表示继承 CPA 配置")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(array, &items); err != nil || len(items) > 100 {
		return nil, fail(400, "rules 最多包含 100 条规则")
	}
	rules := make([]RequestErrorRule, 0, len(items))
	for i, item := range items {
		var rule RequestErrorRule
		if err := strictErrorRulesObject(item, &rule); err != nil {
			return nil, fail(400, fmt.Sprintf("第 %d 条规则格式错误或包含未知字段", i+1))
		}
		if rule.Status < 100 || rule.Status > 599 {
			return nil, fail(400, fmt.Sprintf("第 %d 条规则 status 必须是 100..599 的整数", i+1))
		}
		rule.Action = strings.ToLower(strings.TrimSpace(rule.Action))
		switch rule.Action {
		case "stop", "stop-and-cooldown", "continue", "continue-and-cooldown":
		default:
			return nil, fail(400, fmt.Sprintf("第 %d 条规则 action 无效", i+1))
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(item, &fields)
		for _, key := range []string{"match", "match-regexr"} {
			if value, present := fields[key]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fail(400, fmt.Sprintf("第 %d 条规则的匹配字段必须是字符串数组", i+1))
			}
		}
		if len(rule.Match)+len(rule.MatchRegexr) == 0 || len(rule.Match) > 32 || len(rule.MatchRegexr) > 32 {
			return nil, fail(400, fmt.Sprintf("第 %d 条规则至少需要一个匹配项，每类最多 32 项", i+1))
		}
		for _, matches := range [][]string{rule.Match, rule.MatchRegexr} {
			for _, value := range matches {
				if strings.TrimSpace(value) == "" || len(value) > 4096 {
					return nil, fail(400, fmt.Sprintf("第 %d 条规则匹配项须非空且最多 4096 字节", i+1))
				}
			}
		}
		for _, pattern := range rule.MatchRegexr {
			if _, err := regexp.Compile(pattern); err != nil {
				return nil, fail(400, fmt.Sprintf("第 %d 条规则含无效的 Go 正则表达式", i+1))
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func loadErrorRules(dataDir string) ([]RequestErrorRule, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, errorRulesFile))
	if os.IsNotExist(err) {
		return requestErrorRules(), nil
	}
	if err != nil {
		return nil, fail(500, "读取统一错误规则失败")
	}
	rules, err := decodeErrorRules(raw)
	if err != nil {
		return nil, fail(500, "持久化的统一错误规则无效，请检查 error-rules.json")
	}
	return rules, nil
}

// Caller holds mu. Store bounded, fixed errors rather than host callback text,
// which can contain keys, request headers or entire credential JSON.
func (s *Service) markErrorRulesSyncedLocked(credentialID string, rules []RequestErrorRule, err error) {
	if s.errorRuleSyncErrors == nil {
		s.errorRuleSyncErrors = map[string]string{}
	}
	if s.errorRuleSynced == nil {
		s.errorRuleSynced = map[string]string{}
	}
	if err != nil {
		s.errorRuleSyncErrors[credentialID] = "凭据规则同步失败，请重试同步"
		return
	}
	s.errorRuleSynced[credentialID] = string(jsonBytes(rules))
	delete(s.errorRuleSyncErrors, credentialID)
}

func redactErrorRuleIdentity(value string, creds []Credential) string {
	for _, c := range creds {
		if c.APIKey != "" {
			value = strings.ReplaceAll(value, c.APIKey, "[redacted]")
		}
	}
	return value
}

func (s *Service) errorRulesResponse() errorRulesResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rules := cloneErrorRules(s.errorRules)
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		if !s.revoked[c.ID] {
			creds = append(creds, c)
		}
	}
	sortCredentials(creds)
	sync := errorRuleSyncResult{Total: len(creds), Failed: []errorRuleSyncFailure{}}
	for _, c := range creds {
		detail := s.errorRuleSyncErrors[c.ID]
		if detail == "" && s.errorRuleSynced[c.ID] != string(jsonBytes(rules)) {
			detail = "待同步到 CPA 凭据"
		}
		if detail == "" {
			sync.Succeeded++
		} else {
			sync.Failed = append(sync.Failed, errorRuleSyncFailure{
				ID: redactErrorRuleIdentity(c.ID, creds), Label: redactErrorRuleIdentity(c.Label, creds), Error: detail,
			})
		}
	}
	return errorRulesResponse{Rules: rules, Defaults: requestErrorRules(), Sync: sync, Inherits: len(rules) == 0}
}

func (s *Service) errorRulesManagement(r ManagementRequest) (any, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if r.Method == "GET" {
		return managementJSON(200, s.errorRulesResponse())
	}
	var rules []RequestErrorRule
	if r.Method == "PUT" {
		var err error
		rules, err = decodeErrorRules(r.Body)
		if err != nil {
			return managementJSON(400, map[string]any{"error": err.Error()})
		}
	} else {
		var empty struct{}
		if err := strictErrorRulesObject(r.Body, &empty); err != nil {
			return managementJSON(400, map[string]any{"error": "请求体必须是空对象 {}"})
		}
		if strings.HasSuffix(r.Path, "/reset") {
			rules = requestErrorRules()
		}
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	if rules != nil {
		// Match parseAuth's file-write lock while publishing a new policy. All
		// credential writers hold credentialMu and read the current policy.
		s.authFileMu.Lock()
		if err := atomicJSON(filepath.Join(s.config().DataDir, errorRulesFile), errorRulesDocument{Rules: rules}); err != nil {
			s.authFileMu.Unlock()
			return managementJSON(500, map[string]any{"error": "保存统一错误规则失败"})
		}
		s.mu.Lock()
		s.errorRules = cloneErrorRules(rules)
		s.mu.Unlock()
		s.authFileMu.Unlock()
	}
	s.syncErrorRulesLocked()
	return managementJSON(200, s.errorRulesResponse())
}

// Caller holds credentialMu, so a late sampler/model/credential save cannot
// write a previous policy after this operation. Try every account independently.
func (s *Service) syncErrorRulesLocked() {
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		if !s.revoked[c.ID] {
			creds = append(creds, c)
		}
	}
	s.mu.RUnlock()
	sortCredentials(creds)
	for _, c := range creds {
		rules := s.currentErrorRules()
		err := s.syncCredentialErrorRules(c, rules)
		s.mu.Lock()
		s.markErrorRulesSyncedLocked(c.ID, rules, err)
		s.mu.Unlock()
	}
}

// Change only the rule field, including for disabled or quota-paused accounts.
// Avoid saveCredential's quota normalization and generic rotated registration.
func (s *Service) syncCredentialErrorRules(c Credential, rules []RequestErrorRule) error {
	s.authFileMu.Lock()
	s.mu.RLock()
	current, filename, dir := s.creds[c.ID], s.authFiles[c.ID], s.authDir
	s.mu.RUnlock()
	if !bytes.Equal(jsonBytes(current), jsonBytes(c)) {
		s.authFileMu.Unlock()
		return fail(409, "凭据已变更")
	}
	if current.RoutingID != "" || (dir != "" && filename != "") {
		defer s.authFileMu.Unlock()
		file, err := s.affinityFile(current)
		if err != nil {
			return err
		}
		file.updated["request_scoped_errors"] = jsonBytes(rules)
		latest, err := os.ReadFile(file.path)
		if err != nil || !bytes.Equal(latest, file.original) {
			return fail(409, "凭据文件已变更")
		}
		var old map[string]json.RawMessage
		_ = json.Unmarshal(file.original, &old)
		var compact bytes.Buffer
		_ = json.Compact(&compact, old["request_scoped_errors"])
		if !bytes.Equal(compact.Bytes(), jsonBytes(rules)) {
			if err := atomicJSON(file.path, file.updated); err != nil {
				return err
			}
		}
	} else {
		// host.auth.save can synchronously call parseAuth: never hold the file
		// lock across that callback. Unknown JSON fields survive this path too.
		s.authFileMu.Unlock()
		current.RequestScopedErrors = cloneErrorRules(rules)
		if filename == "" {
			filename = c.ID + ".json"
		}
		if filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
			return fail(400, "凭据文件名无效")
		}
		if err := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(jsonBytes(current))}, nil); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.creds[c.ID]
	if !exists || current.APIKey != c.APIKey {
		return fail(409, "凭据已变更")
	}
	current.RequestScopedErrors = cloneErrorRules(rules)
	s.creds[c.ID] = current
	return nil
}

// Caller holds authFileMu. On restart and watcher imports, register the unified
// policy and reconcile only its physical rule field; keep all other JSON intact.
func (s *Service) applyParsedErrorRules(c *Credential, dir, filename string, raw []byte) {
	rules := s.currentErrorRules()
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var old []RequestErrorRule
	_ = json.Unmarshal(fields["request_scoped_errors"], &old)
	var err error
	if !bytes.Equal(jsonBytes(old), jsonBytes(rules)) {
		_, policyErr := os.Stat(filepath.Join(s.config().DataDir, errorRulesFile))
		if policyErr != nil || dir == "" || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
			err = errors.New("unresolved credential file")
		} else {
			path := filepath.Join(dir, filename)
			latest, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(latest, raw) {
				err = errors.New("credential file changed")
			} else {
				fields["request_scoped_errors"] = jsonBytes(rules)
				err = atomicJSON(path, fields)
			}
		}
	}
	c.RequestScopedErrors = cloneErrorRules(rules)
	s.mu.Lock()
	s.markErrorRulesSyncedLocked(c.ID, rules, err)
	s.mu.Unlock()
}
