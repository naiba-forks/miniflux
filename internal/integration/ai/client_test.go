// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSummarizeResultToleratesCommonModelFormatting(t *testing.T) {
	tests := []struct {
		name    string
		content string
		summary string
		score   int
	}{
		{
			name:    "plain JSON",
			content: `{"summary":"Plain response","score":5}`,
			summary: "Plain response",
			score:   5,
		},
		{
			name:    "JSON Markdown fence",
			content: "```json\n{\n  \"summary\": \"Fenced response\",\n  \"score\": 8\n}\n```",
			summary: "Fenced response",
			score:   8,
		},
		{
			name:    "generic Markdown fence after thinking",
			content: "<think>internal reasoning</think>\n```\n{\"summary\":\"Safe response\",\"score\":7}\n```",
			summary: "Safe response",
			score:   7,
		},
		{
			name:    "preamble with unrelated braces",
			content: "Here is {not JSON}. Example: {}. Final answer: {\"summary\":\"Recovered response\",\"score\":6}",
			summary: "Recovered response",
			score:   6,
		},
		{
			name:    "Unicode JSON whitespace and trailing comma",
			content: "```json\n{\n\u00a0 \"summary\": \"Unicode whitespace\",\n\u00a0 \"score\": 4,\n}\n```",
			summary: "Unicode whitespace",
			score:   4,
		},
		{
			name:    "content punctuation remains unchanged",
			content: "```json\n{\"summary\":\"Keep ```code```, {braces}, comma,} and\u00a0space\",\"score\":9}\n```",
			summary: "Keep ```code```, {braces}, comma,} and\u00a0space",
			score:   9,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := parseSummarizeResult(test.content)
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if result.Summary != test.summary || result.Score != test.score {
				t.Fatalf("unexpected result: %#v", result)
			}
		})
	}
}

func TestParseSummarizeResultRejectsInvalidResponses(t *testing.T) {
	for _, content := range []string{
		"not JSON",
		`{"summary":"","score":5}`,
		`{"message":"missing summary"}`,
		`{"summary":"missing score"}`,
	} {
		t.Run(content, func(t *testing.T) {
			if _, err := parseSummarizeResult(content); err == nil {
				t.Fatalf("expected invalid response to fail: %q", content)
			}
		})
	}
}

func TestParseSummarizeResultUsesJSONRepairFallback(t *testing.T) {
	tests := []struct {
		name    string
		content string
		summary string
		score   int
	}{
		{
			name:    "unquoted keys and single quoted values",
			content: `{summary: 'Repaired response', score: 5}`,
			summary: "Repaired response",
			score:   5,
		},
		{
			name:    "missing comma",
			content: `{"summary":"Missing comma" "score":6}`,
			summary: "Missing comma",
			score:   6,
		},
		{
			name:    "truncated object",
			content: `{"summary":"Truncated response","score":7`,
			summary: "Truncated response",
			score:   7,
		},
		{
			name:    "JavaScript comment",
			content: "{\n// generated result\n\"summary\":\"Comment removed\",\n\"score\":8\n}",
			summary: "Comment removed",
			score:   8,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := parseSummarizeResult(test.content)
			if err != nil {
				t.Fatalf("unexpected repair error: %v", err)
			}
			if result.Summary != test.summary || result.Score != test.score {
				t.Fatalf("unexpected repaired result: %#v", result)
			}
		})
	}
}

func TestParseSummarizeResultAcceptsNumericStringScore(t *testing.T) {
	result, err := parseSummarizeResult(`{"summary":"String score","score":"9"}`)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if result.Score != 9 {
		t.Fatalf("unexpected score: %d", result.Score)
	}
}

func TestParseSummarizeResultRejectsOversizedInput(t *testing.T) {
	content := `{"summary":"` + strings.Repeat("x", maxModelJSONLength) + `","score":5}`
	if _, err := parseSummarizeResult(content); err == nil {
		t.Fatal("expected oversized model response to be rejected")
	}
}

func TestParseSummarizeResultDoesNotChangeWhitespaceInsideSummary(t *testing.T) {
	result, err := parseSummarizeResult(`{"summary":"line 1\nline 2\tend","score":5}`)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !strings.Contains(result.Summary, "\n") || !strings.Contains(result.Summary, "\t") {
		t.Fatalf("expected escaped whitespace to be preserved: %q", result.Summary)
	}
}

func TestConnectionChecksCompatibleCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected endpoint: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}

		var request chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("unable to decode request: %v", err)
		}
		if request.Model != "test-model" {
			t.Fatalf("unexpected model: %q", request.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"OK\",\"score\":10}"}}]}`))
	}))
	defer server.Close()

	if err := NewClient(server.URL, "secret", "test-model").TestConnection(); err != nil {
		t.Fatalf("unexpected connection test error: %v", err)
	}
}

func TestConnectionReturnsProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid API key", http.StatusUnauthorized)
	}))
	defer server.Close()

	if err := NewClient(server.URL, "bad-secret", "test-model").TestConnection(); err == nil {
		t.Fatal("expected a provider error")
	}
}
