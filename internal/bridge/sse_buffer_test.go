package bridge

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestCompletionBufferedStringsPreserveFunctionsAndSnapshots(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			completion := newCompletion()
			functionDelta := func(name, arguments, note string) map[string]any {
				function := map[string]any{"name": name, "arguments": arguments, "extension": map[string]any{"note": note}}
				if legacy {
					return map[string]any{"function_call": function}
				}
				return map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": function}}}
			}
			observe := func(delta map[string]any, finish any) {
				completion.observe(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			}
			prefix := "<tag>&\x00\"\\\u2028"
			first := functionDelta("look", `{"q":"`, "a")
			for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
				first[key] = prefix
			}
			observe(first, nil)
			message := object(completion.choices[0]["message"])
			snapshot := completionString(message["content"])
			fragment := strings.Repeat("x", 64)
			for range 2000 {
				observe(map[string]any{"content": fragment, "reasoning_content": fragment, "reasoning": fragment, "refusal": fragment}, nil)
			}
			finish := "tool_calls"
			if legacy {
				finish = "function_call"
			}
			observe(functionDelta("up", `query"}`, "b"), finish)
			if snapshot != prefix || !completion.completeAtEOF() {
				t.Fatalf("snapshot changed or fragmented function failed EOF validation: snapshot=%q complete=%t", snapshot, completion.completeAtEOF())
			}
			completion.done = true
			encoded, err := completion.result("model")
			if err != nil {
				t.Fatal(err)
			}
			root, err := decodeObject(encoded)
			if err != nil {
				t.Fatal(err)
			}
			result := object(object(list(root["choices"])[0])["message"])
			want := prefix + strings.Repeat(fragment, 2000)
			for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
				if result[key] != want {
					t.Fatalf("%s lost accumulated text", key)
				}
			}
			function := object(result["function_call"])
			if !legacy {
				function = object(object(list(result["tool_calls"])[0])["function"])
			}
			if function["name"] != "lookup" || function["arguments"] != `{"q":"query"}` || object(function["extension"])["note"] != "ab" {
				t.Fatalf("fragmented function fields lost: %#v", function)
			}
			// Reusing the aggregate must not change already emitted JSON or a
			// previously obtained Builder.String snapshot.
			oldJSON := bytes.Clone(encoded)
			observe(map[string]any{"content": "tail"}, nil)
			if _, err := completion.result("model"); err != nil || !bytes.Equal(encoded, oldJSON) || snapshot != prefix {
				t.Fatal("later appends changed a previous output snapshot")
			}
		})
	}
}

func TestCompletionBufferedMergePreservesNullsAndTypeChanges(t *testing.T) {
	destination := map[string]any{"text": "prefix", "nested": map[string]any{"value": "first"}}
	mergeObjects(destination, map[string]any{"text": "-suffix", "nested": map[string]any{"value": nil}}, true)
	mergeObjects(destination, map[string]any{"text": 7, "nested": map[string]any{"value": "-second"}}, true)
	mergeObjects(destination, map[string]any{"text": "new"}, true)
	if got := string(jsonBytes(destination)); got != `{"nested":{"value":"first-second"},"text":"new"}` {
		t.Fatalf("concatenation changed null/type replacement semantics: %s", got)
	}
	mergeObjects(destination, map[string]any{"text": map[string]any{"field": "nested"}}, true)
	mergeObjects(destination, map[string]any{"text": map[string]any{"field": "-value"}}, true)
	if completionString(object(destination["text"])["field"]) != "nested-value" {
		t.Fatal("string to object replacement lost nested concatenation")
	}
}

func BenchmarkCompletionLongDeltas(b *testing.B) {
	for _, chunks := range []int{1000, 2000, 4000, 8000} {
		b.Run(fmt.Sprintf("chunks=%d", chunks), func(b *testing.B) {
			frame := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": strings.Repeat("x", 64)}}}}
			b.ReportAllocs()
			b.SetBytes(int64(chunks * 64))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				completion := newCompletion()
				for range chunks {
					completion.observe(frame)
				}
				runtime.KeepAlive(completion)
			}
		})
	}
}
