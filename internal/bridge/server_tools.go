package bridge

import (
	"strings"
)

// ServerToolRule appends upstream-executed tools to every request whose model
// matches one of Models.
//
// A server tool is resolved by the upstream gateway itself: the Cline API runs
// on Vercel AI Gateway, which executes built-ins such as "vercel:exa_search" or
// "vercel:perplexity_search" and feeds the hits back to the model inside the
// same turn. The client therefore never receives a tool call and needs no
// local search implementation; it only has to be the one who declares the tool.
//
// Declaring it here keeps that declaration in the bridge, so every client of the
// mapped model gets search without repeating the tool list in each request.
//
// Models holds glob patterns in which '*' matches any run of characters, so one
// rule covers a family such as "deepseek-v4.1-flash-*". Tools are the raw
// Chat Completions tool objects to append, for example
// {"type":"vercel:exa_search"}.
type ServerToolRule struct {
	Models []string         `json:"models" yaml:"models"`
	Tools  []map[string]any `json:"tools" yaml:"tools"`
}

// maxServerToolsPerRule bounds a rule so a mistyped settings file cannot grow
// every request without limit.
const maxServerToolsPerRule = 16

func (r *ServerToolRule) validate() error {
	if len(r.Models) == 0 || len(r.Tools) == 0 {
		return fail(400, "server_tools rules need at least one model pattern and one tool")
	}
	for _, pattern := range r.Models {
		if strings.TrimSpace(pattern) == "" || strings.ContainsAny(pattern, "\r\n\t") {
			return fail(400, "server_tools model patterns must be nonempty and contain no control whitespace")
		}
	}
	if len(r.Tools) > maxServerToolsPerRule {
		return fail(400, "a server_tools rule can hold at most 16 tools")
	}
	seen := map[string]bool{}
	for _, tool := range r.Tools {
		if tool == nil || strings.TrimSpace(str(tool["type"])) == "" {
			return fail(400, "server_tools entries must be objects with a nonempty type")
		}
		key := serverToolKey(tool)
		if seen[key] {
			return fail(400, "server_tools entries must be unique within a rule")
		}
		seen[key] = true
	}
	return nil
}

// serverToolKey identifies a tool for de-duplication. Server tools are keyed by
// their type, function tools by the function name, so a client that already
// declared the same search tool is never handed it twice.
func serverToolKey(tool map[string]any) string {
	kind := strings.TrimSpace(str(tool["type"]))
	name := strings.TrimSpace(str(tool["name"]))
	if name == "" {
		name = strings.TrimSpace(str(object(tool["function"])["name"]))
	}
	if kind == "" && name == "" {
		return ""
	}
	return kind + "\x00" + name
}

// builtinServerToolFunctionName names the function reserved by a known gateway
// tool. Unknown types may use a different namespace and must not be inferred.
func builtinServerToolFunctionName(tool map[string]any) string {
	kind := strings.TrimSpace(str(tool["type"]))
	switch kind {
	case "vercel:exa_search", "vercel:perplexity_search", "vercel:parallel_search",
		"vercel:tako_search", "vercel:browserbase_search", "vercel:browserbase_fetch":
		return strings.TrimPrefix(kind, "vercel:")
	default:
		return ""
	}
}

// modelPatternMatches is the glob matcher the rules use; only '*' is special.
func modelPatternMatches(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	model = strings.TrimSpace(model)
	if pattern == "" || model == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	p, m := 0, 0
	star, resume := -1, 0
	for m < len(model) {
		if p < len(pattern) && (pattern[p] == model[m]) {
			p++
			m++
			continue
		}
		if p < len(pattern) && pattern[p] == '*' {
			star = p
			resume = m
			p++
			continue
		}
		if star < 0 {
			return false
		}
		p = star + 1
		resume++
		m = resume
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// applyServerTools appends the configured server tools for model to the outgoing
// request body. Tools the caller already declared are left alone: an identical
// type (and name) is skipped rather than duplicated, so a client keeps its own
// definitions and ordering. A client function also takes precedence over a
// built-in gateway tool that reserves the same function name.
func (s *Service) applyServerTools(j map[string]any, model string) {
	cfg := s.config()
	if len(cfg.ServerTools) == 0 || strings.TrimSpace(model) == "" {
		return
	}
	tools := list(j["tools"])
	existing := make(map[string]bool, len(tools))
	clientFunctions := make(map[string]bool)
	for _, entry := range tools {
		if tool := object(entry); tool != nil {
			if key := serverToolKey(tool); key != "" {
				existing[key] = true
			}
			if strings.TrimSpace(str(tool["type"])) == "function" {
				name := strings.TrimSpace(str(object(tool["function"])["name"]))
				if name == "" {
					name = strings.TrimSpace(str(tool["name"]))
				}
				if name != "" {
					clientFunctions[name] = true
				}
			}
		}
	}
	appended := false
	for i := range cfg.ServerTools {
		rule := &cfg.ServerTools[i]
		if !serverToolRuleMatches(rule, model) {
			continue
		}
		for _, tool := range rule.Tools {
			key := serverToolKey(tool)
			if key == "" || existing[key] {
				continue
			}
			if name := builtinServerToolFunctionName(tool); name != "" && clientFunctions[name] {
				continue
			}
			tools = append(tools, tool)
			existing[key] = true
			appended = true
		}
	}
	if appended {
		j["tools"] = tools
	}
}

func serverToolRuleMatches(rule *ServerToolRule, model string) bool {
	for _, pattern := range rule.Models {
		if modelPatternMatches(pattern, model) {
			return true
		}
	}
	return false
}
