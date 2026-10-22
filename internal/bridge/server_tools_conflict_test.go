package bridge

import (
	"fmt"
	"reflect"
	"testing"
)

func TestServerToolsSkipConflictingClientFunctions(t *testing.T) {
	for _, name := range []string{
		"exa_search", "perplexity_search", "parallel_search", "tako_search",
		"browserbase_search", "browserbase_fetch",
	} {
		t.Run(name, func(t *testing.T) {
			rules := fmt.Sprintf("  - models: [\"deepseek-flash\"]\n    tools:\n      - type: vercel:%s\n      - type: vercel:future_search\n", name)
			s := serverToolsService(t, rules)
			client := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": name, "description": "Client-owned search", "strict": true,
					"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}
			payload := map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
				"tools":    []any{client},
			}
			body := outboundBody(t, s, string(jsonBytes(payload)))
			tools := list(body["tools"])
			want := []string{"function", "vercel:future_search"}
			if got := toolTypes(body); !reflect.DeepEqual(got, want) {
				t.Fatalf("tools = %v, want %v", got, want)
			}
			if !reflect.DeepEqual(tools[0], client) {
				t.Fatalf("client function was changed: %v", tools[0])
			}
		})
	}
}

func TestServerToolsKeepNonconflictingTools(t *testing.T) {
	rules := "  - models: [\"deepseek-flash\"]\n    tools:\n      - type: vercel:exa_search\n      - type: vercel:future_search\n"
	s := serverToolsService(t, rules)
	// An unknown server tool need not reserve its type suffix as a function name.
	body := outboundBody(t, s, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"future_search","parameters":{"type":"object"}}}]}`)
	want := []string{"function", "vercel:exa_search", "vercel:future_search"}
	if got := toolTypes(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
}

func TestServerToolsKeepDeclaredBuiltinTools(t *testing.T) {
	rules := "  - models: [\"deepseek-flash\"]\n    tools:\n      - type: vercel:exa_search\n      - type: vercel:tako_search\n" +
		"  - models: [\"deepseek-*\"]\n    tools:\n      - type: vercel:exa_search\n      - type: vercel:tako_search\n"
	s := serverToolsService(t, rules)
	body := outboundBody(t, s, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"vercel:exa_search","options":{"category":"news"}}]}`)
	want := []string{"vercel:exa_search", "vercel:tako_search"}
	if got := toolTypes(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	client := map[string]any{"type": "vercel:exa_search", "options": map[string]any{"category": "news"}}
	if got := list(body["tools"])[0]; !reflect.DeepEqual(got, client) {
		t.Fatalf("client server tool was changed: %v", got)
	}
}
