package bridge

import (
	"encoding/json"
	"strings"
)

// serverToolCatalog is what the console offers per model. It mirrors the
// server-tool set the Cline API accepts on Chat Completions through AI Gateway;
// operators can still type a type the catalog does not know about.
var serverToolCatalog = []map[string]string{
	{"type": "vercel:exa_search", "label": "Exa 搜索", "note": "语义检索，支持站点、日期和内容类别过滤；$7/千次（instant $4/千次）。"},
	{"type": "vercel:perplexity_search", "label": "Perplexity 搜索", "note": "研究型检索，支持时效与域名过滤；$5/千次。"},
	{"type": "vercel:parallel_search", "label": "Parallel 搜索", "note": "面向 objective 的任务式调研；$5/千次（含 10 条结果）。"},
	{"type": "vercel:tako_search", "label": "Tako 搜索", "note": "网页加实时知识图谱（金融、体育、天气）；$7/千次，deep $12/千次。"},
	{"type": "vercel:browserbase_search", "label": "Browserbase 搜索", "note": "快速发现页面，通常配合抓取使用；$7/千次。"},
	{"type": "vercel:browserbase_fetch", "label": "Browserbase 抓取", "note": "按 URL 抓正文，raw $1/千次起（代理或抽取另计）。"},
}

// modelServerTools is the console-facing shape: one tool-type list per
// configured model id, plus the catalog to render.
type modelServerTools struct {
	Models  map[string][]string `json:"models"`
	Catalog []map[string]string `json:"catalog"`
}

// effectiveServerTools resolves the rules to the tools each configured model
// actually receives. The console edits this per-model view; the rules stay the
// storage format so a deployment can still be configured from YAML.
func (c Config) effectiveServerTools() map[string][]string {
	out := make(map[string][]string, len(c.Models))
	for _, model := range c.Models {
		seen := map[string]bool{}
		var types []string
		for _, tool := range c.resolvedServerTools(model.ID) {
			kind := strings.TrimSpace(str(tool["type"]))
			if kind == "" || seen[kind] {
				continue
			}
			seen[kind] = true
			types = append(types, kind)
		}
		out[model.ID] = types
	}
	return out
}

// Resolve the complete objects using the same first-definition-wins identity
// as request injection. A type can have several independently named tools.
func (c Config) resolvedServerTools(model string) []map[string]any {
	var tools []map[string]any
	seen := map[string]bool{}
	for _, rule := range c.ServerTools {
		if !serverToolRuleMatches(&rule, model) {
			continue
		}
		for _, tool := range rule.Tools {
			key := serverToolKey(tool)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			tools = append(tools, tool)
		}
	}
	return tools
}

// flattenServerToolRules rewrites the rules as one exact rule per model that has
// tools. Editing one model in the console therefore commits the whole resolved
// view. Existing types retain all their complete objects; only newly selected
// types get a bare declaration.
func (c *Config) flattenServerToolRules(effective map[string][]string) {
	rules := make([]ServerToolRule, 0, len(effective))
	for _, model := range c.Models {
		types := effective[model.ID]
		if len(types) == 0 {
			continue
		}
		selected := make(map[string]bool, len(types))
		for _, kind := range types {
			selected[kind] = true
		}
		tools := make([]map[string]any, 0, len(types))
		retained := map[string]bool{}
		for _, tool := range c.resolvedServerTools(model.ID) {
			kind := strings.TrimSpace(str(tool["type"]))
			if selected[kind] {
				tools = append(tools, tool)
				retained[kind] = true
			}
		}
		for _, kind := range types {
			if !retained[kind] {
				tools = append(tools, map[string]any{"type": kind})
			}
		}
		rules = append(rules, ServerToolRule{Models: []string{model.ID}, Tools: tools})
	}
	c.ServerTools = rules
}

func (s *Service) modelServerToolsResponse() (any, error) {
	cfg := s.config()
	effective := cfg.effectiveServerTools()
	if effective == nil {
		effective = map[string][]string{}
	}
	return managementJSON(200, modelServerTools{Models: effective, Catalog: serverToolCatalog})
}

// saveModelServerTools replaces the tool list of one model. Unknown models are
// rejected so a stale console cannot write rules for a deleted model.
func (s *Service) saveModelServerTools(r ManagementRequest) (any, error) {
	var in struct {
		Model string   `json:"model"`
		Tools []string `json:"tools"`
	}
	if e := json.Unmarshal(r.Body, &in); e != nil || strings.TrimSpace(in.Model) == "" {
		return managementJSON(400, map[string]any{"error": "invalid server tools JSON"})
	}
	cfg := s.config()
	found := false
	for _, model := range cfg.Models {
		if model.ID == in.Model {
			found = true
			break
		}
	}
	if !found {
		return managementJSON(404, map[string]any{"error": "模型不存在，请刷新后重试"})
	}
	effective := cfg.effectiveServerTools()
	seen := map[string]bool{}
	types := make([]string, 0, len(in.Tools))
	for _, kind := range in.Tools {
		trimmed := strings.TrimSpace(kind)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		types = append(types, trimmed)
	}
	effective[in.Model] = types
	cfg.flattenServerToolRules(effective)
	if e := s.saveConfig(cfg); e != nil {
		return managementJSON(statusOf(e), map[string]any{"error": safeError(e)})
	}
	return managementJSON(200, map[string]any{"models": cfg.effectiveServerTools(), "catalog": serverToolCatalog, "message": "已保存，下一个请求开始生效。"})
}
