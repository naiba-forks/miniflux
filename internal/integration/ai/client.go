// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai // import "miniflux.app/v2/internal/integration/ai"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kaptinlin/jsonrepair"
)

const (
	defaultClientTimeout = 30 * time.Second
	maxContentLength     = 4000
	maxModelJSONLength   = 64 * 1024
)

var htmlTagRegexp = regexp.MustCompile("<[^>]*>")

// thinkingTagRegexp matches <think>...</think> blocks that some AI models (e.g. DeepSeek, QWen) include
// in their responses. These contain internal reasoning that must be stripped before using the content.
var thinkingTagRegexp = regexp.MustCompile(`(?s)<think>.*?</think>`)

// Client communicates with an OpenAI-compatible chat completions API.
type Client struct {
	providerURL string // e.g. "https://api.openai.com/v1"
	apiKey      string
	model       string
}

// NewClient creates a new AI client for the given OpenAI-compatible provider.
func NewClient(providerURL, apiKey, model string) *Client {
	return &Client{
		providerURL: providerURL,
		apiKey:      apiKey,
		model:       model,
	}
}

// SummarizeResult holds the AI-generated summary and score for an article.
type SummarizeResult struct {
	Summary string `json:"summary"`
	Score   int    `json:"score"`
}

// SummarizeEntry sends article content to the AI provider and returns a summary with a 1-10 score.
// It calls the OpenAI-compatible /chat/completions endpoint.
// The content is truncated to ~4000 chars to control token usage.
// If the entry already has a summary (non-empty aiSummary), it returns nil to avoid wasting tokens.
// The language parameter controls the summary output language (e.g. "en_US", "zh_CN").
func (c *Client) SummarizeEntry(title, content, aiSummary, language string) (*SummarizeResult, error) {
	// Skip if already summarized — avoid duplicate API calls and wasted tokens.
	if aiSummary != "" {
		return nil, nil
	}

	return c.callSummarize(title, content, language)
}

// ForceSummarizeEntry always calls the AI provider, ignoring any existing summary.
// Used by the force-backfill feature to regenerate summaries with a new model or language.
func (c *Client) ForceSummarizeEntry(title, content, language string) (*SummarizeResult, error) {
	return c.callSummarize(title, content, language)
}

// TestConnection verifies the complete OpenAI-compatible summarization path,
// including authentication, model availability, and the expected JSON output.
func (c *Client) TestConnection() error {
	_, err := c.callSummarize("Connection test", "This is a short connection test.", "en_US")
	return err
}

// buildSystemPrompt constructs the system prompt with the user's preferred language.
// The language code (e.g. "zh_CN", "en_US") is mapped to a human-readable name.
func buildSystemPrompt(language string) string {
	// Map locale codes to language names the AI model understands.
	langName := "the same language as the article"
	switch {
	case strings.HasPrefix(language, "zh"):
		langName = "Simplified Chinese (中文)"
	case strings.HasPrefix(language, "ja"):
		langName = "Japanese"
	case strings.HasPrefix(language, "ko"):
		langName = "Korean"
	case strings.HasPrefix(language, "de"):
		langName = "German"
	case strings.HasPrefix(language, "fr"):
		langName = "French"
	case strings.HasPrefix(language, "es"):
		langName = "Spanish"
	case strings.HasPrefix(language, "pt"):
		langName = "Portuguese"
	case strings.HasPrefix(language, "ru"):
		langName = "Russian"
	case strings.HasPrefix(language, "ar"):
		langName = "Arabic"
	case strings.HasPrefix(language, "en"):
		langName = "English"
	}

	// Score by content quality (depth, originality, usefulness), NOT by article category/type.
	// A well-written tutorial or review can score just as high as breaking news.
	return "You are a content analyzer. For the given article, provide:\n" +
		"1. A concise summary (2-3 sentences) in " + langName + ". Adapt style to content type: for news, state the key facts and impact; for product reviews, highlight standout specs and verdict; for tutorials, state what is taught and key takeaway.\n" +
		"2. A quality score from 1 to 10 based on the article's CONTENT QUALITY, not its category or type. Any category (news, tutorial, review, opinion) can score high or low:\n" +
		"   - 9-10: Exceptional depth, original insights or data, highly actionable, well-researched with primary sources\n" +
		"   - 7-8: Solid depth, provides useful analysis or practical guidance, good supporting evidence\n" +
		"   - 5-6: Adequate coverage, some useful information but mostly surface-level or commonly known\n" +
		"   - 3-4: Thin content, mostly rehashed from other sources, little original value\n" +
		"   - 1-2: Minimal substance, clickbait, or purely promotional with no real information\n" +
		"Respond ONLY with JSON: {\"summary\": \"...\", \"score\": N}"
}

// callSummarize is the shared implementation for SummarizeEntry and ForceSummarizeEntry.
func (c *Client) callSummarize(title, content, language string) (*SummarizeResult, error) {
	cleanContent := truncateContent(stripHTMLTags(content), maxContentLength)
	userMessage := title + "\n\n" + cleanContent

	requestPayload := chatCompletionRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: buildSystemPrompt(language)},
			{Role: "user", Content: userMessage},
		},
		Temperature: 0.3,
	}

	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, fmt.Errorf("ai: unable to encode request body: %v", err)
	}

	apiEndpoint := strings.TrimRight(c.providerURL, "/") + "/chat/completions"
	request, err := http.NewRequest(http.MethodPost, apiEndpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("ai: unable to create request: %v", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiKey)

	// Use system proxy settings (HTTP_PROXY, HTTPS_PROXY, NO_PROXY env vars).
	httpClient := &http.Client{
		Timeout: defaultClientTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("ai: unable to send request: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("ai: unable to read response body: %v", err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ai: provider returned status %d: %s", response.StatusCode, truncateContent(string(responseBody), 512))
	}

	var completionResp chatCompletionResponse
	if err := json.Unmarshal(responseBody, &completionResp); err != nil {
		return nil, fmt.Errorf("ai: unable to parse response JSON: %v", err)
	}

	if len(completionResp.Choices) == 0 {
		return nil, fmt.Errorf("ai: response contains no choices")
	}

	messageContent := stripThinkingContent(strings.TrimSpace(completionResp.Choices[0].Message.Content))
	if messageContent == "" {
		return nil, fmt.Errorf("ai: response message content is empty")
	}

	// Models sometimes wrap otherwise valid JSON in Markdown fences or add a
	// short preamble despite being instructed to return JSON only.
	result, err := parseSummarizeResult(messageContent)
	if err != nil {
		return nil, fmt.Errorf("ai: unable to parse summary JSON from model response: %v (raw: %s)", err, truncateContent(messageContent, 256))
	}

	// Clamp score to valid 1-10 range to handle model hallucinations.
	if result.Score < 1 {
		result.Score = 1
	}
	if result.Score > 10 {
		result.Score = 10
	}

	return result, nil
}

// parseSummarizeResult extracts the first valid summary object from a model
// response. It accepts plain JSON, Markdown fenced JSON, and a short prose
// preamble/suffix. Each opening brace is tried as a candidate so braces in a
// preamble do not prevent a later valid object from being decoded.
func parseSummarizeResult(content string) (*SummarizeResult, error) {
	content = stripThinkingContent(strings.TrimSpace(content))
	if len(content) > maxModelJSONLength {
		return nil, fmt.Errorf("model response is too large (%d bytes)", len(content))
	}

	normalized := normalizeModelJSONWhitespace(content)
	normalized = removeModelJSONTrailingCommas(normalized)
	result, parseErr := extractSummarizeResult(normalized)
	if parseErr == nil {
		return result, nil
	}

	repaired, repairErr := jsonrepair.Repair(content)
	if repairErr != nil {
		return nil, fmt.Errorf("standard parse failed: %v; JSON repair failed: %v", parseErr, repairErr)
	}

	result, repairedParseErr := extractSummarizeResult(repaired)
	if repairedParseErr != nil {
		return nil, fmt.Errorf("standard parse failed: %v; repaired JSON is invalid: %v", parseErr, repairedParseErr)
	}
	return result, nil
}

func extractSummarizeResult(content string) (*SummarizeResult, error) {

	var lastErr error
	for offset := 0; offset < len(content); {
		brace := strings.IndexByte(content[offset:], '{')
		if brace < 0 {
			break
		}
		offset += brace

		result, err := decodeSummarizeResult(strings.NewReader(content[offset:]))
		if err == nil {
			return result, nil
		} else {
			lastErr = err
		}
		offset++
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no JSON object found")
	}
	return nil, lastErr
}

func decodeSummarizeResult(reader *strings.Reader) (*SummarizeResult, error) {
	var payload struct {
		Summary string          `json:"summary"`
		Score   json.RawMessage `json:"score"`
	}
	if err := json.NewDecoder(reader).Decode(&payload); err != nil {
		return nil, err
	}
	if strings.TrimSpace(payload.Summary) == "" {
		return nil, fmt.Errorf("summary is empty")
	}
	if len(payload.Score) == 0 {
		return nil, fmt.Errorf("score is missing")
	}

	var score int
	if err := json.Unmarshal(payload.Score, &score); err != nil {
		var stringScore string
		if stringErr := json.Unmarshal(payload.Score, &stringScore); stringErr != nil {
			return nil, fmt.Errorf("score must be an integer: %v", err)
		}
		parsedScore, stringErr := strconv.Atoi(strings.TrimSpace(stringScore))
		if stringErr != nil {
			return nil, fmt.Errorf("score must be an integer: %v", stringErr)
		}
		score = parsedScore
	}

	return &SummarizeResult{Summary: payload.Summary, Score: score}, nil
}

// normalizeModelJSONWhitespace converts Unicode whitespace outside JSON
// strings to regular spaces. Some models indent fenced JSON with non-breaking
// spaces, which encoding/json correctly rejects as invalid JSON whitespace.
func normalizeModelJSONWhitespace(content string) string {
	var builder strings.Builder
	builder.Grow(len(content))
	inString := false
	escaped := false

	for _, char := range content {
		if inString {
			builder.WriteRune(char)
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}

		if char == '"' {
			inString = true
			builder.WriteRune(char)
		} else if unicode.IsSpace(char) {
			builder.WriteByte(' ')
		} else {
			builder.WriteRune(char)
		}
	}

	return builder.String()
}

// removeModelJSONTrailingCommas removes commas immediately before an object or
// array close, but only outside strings. This handles another common model JSON
// formatting mistake without modifying article text.
func removeModelJSONTrailingCommas(content string) string {
	runes := []rune(content)
	var builder strings.Builder
	builder.Grow(len(content))
	inString := false
	escaped := false

	for index, char := range runes {
		if inString {
			builder.WriteRune(char)
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}

		if char == '"' {
			inString = true
			builder.WriteRune(char)
			continue
		}

		if char == ',' {
			next := index + 1
			for next < len(runes) && unicode.IsSpace(runes[next]) {
				next++
			}
			if next < len(runes) && (runes[next] == '}' || runes[next] == ']') {
				continue
			}
		}

		builder.WriteRune(char)
	}

	return builder.String()
}

// buildPageSummaryPrompt constructs the system prompt for generating a combined digest summary.
func buildPageSummaryPrompt(language string) string {
	langName := "the same language as the articles"
	switch {
	case strings.HasPrefix(language, "zh"):
		langName = "Simplified Chinese (中文)"
	case strings.HasPrefix(language, "ja"):
		langName = "Japanese"
	case strings.HasPrefix(language, "ko"):
		langName = "Korean"
	case strings.HasPrefix(language, "de"):
		langName = "German"
	case strings.HasPrefix(language, "fr"):
		langName = "French"
	case strings.HasPrefix(language, "es"):
		langName = "Spanish"
	case strings.HasPrefix(language, "pt"):
		langName = "Portuguese"
	case strings.HasPrefix(language, "ru"):
		langName = "Russian"
	case strings.HasPrefix(language, "ar"):
		langName = "Arabic"
	case strings.HasPrefix(language, "en"):
		langName = "English"
	}

	// MUST cover ALL articles — never skip any. Previous prompt told AI to skip low-scored items,
	// causing users to see only ~half the articles in the digest.
	// MUST output plain text only — no markdown syntax (no #, *, -, >, ```, etc.).
	// The output is used for browser TTS (SpeechSynthesis), so markdown formatting
	// would be read aloud as literal characters, ruining the listening experience.
	return "You are a news digest writer. You will receive a list of article summaries, each prefixed with its source in the format:\n" +
		"[Source: <feed name>] <article title>: <summary>\n\n" +
		"Your task: produce a cohesive overall digest in " + langName + ".\n\n" +
		"Requirements:\n" +
		"1. Group articles by their source. Use the source name as a section header.\n" +
		"2. Cover ALL articles provided. Do not skip any.\n" +
		"3. Clearly attribute information to the correct source.\n" +
		"4. Output PLAIN TEXT only. Do NOT use any Markdown formatting " +
		"(no #, *, -, >, ```, numbered lists with dots, or any other Markdown syntax). " +
		"Use natural paragraph breaks only.\n" +
		"Respond with the digest text only, no JSON wrapper."
}

// GeneratePageSummary takes concatenated article summaries and produces a combined digest.
func (c *Client) GeneratePageSummary(combinedSummaries, language string) (string, error) {
	requestPayload := chatCompletionRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: buildPageSummaryPrompt(language)},
			{Role: "user", Content: truncateContent(combinedSummaries, maxContentLength*8)},
		},
		Temperature: 0.3,
	}

	requestBody, err := json.Marshal(requestPayload)
	if err != nil {
		return "", fmt.Errorf("ai: unable to encode request body: %v", err)
	}

	apiEndpoint := strings.TrimRight(c.providerURL, "/") + "/chat/completions"
	request, err := http.NewRequest(http.MethodPost, apiEndpoint, bytes.NewReader(requestBody))
	if err != nil {
		return "", fmt.Errorf("ai: unable to create request: %v", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiKey)

	httpClient := &http.Client{
		Timeout: defaultClientTimeout * 4,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("ai: unable to send request: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("ai: unable to read response body: %v", err)
	}

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai: provider returned status %d: %s", response.StatusCode, truncateContent(string(responseBody), 512))
	}

	var completionResp chatCompletionResponse
	if err := json.Unmarshal(responseBody, &completionResp); err != nil {
		return "", fmt.Errorf("ai: unable to parse response JSON: %v", err)
	}

	if len(completionResp.Choices) == 0 {
		return "", fmt.Errorf("ai: response contains no choices")
	}

	messageContent := stripThinkingContent(strings.TrimSpace(completionResp.Choices[0].Message.Content))
	if messageContent == "" {
		return "", fmt.Errorf("ai: response message content is empty")
	}

	return messageContent, nil
}

// stripHTMLTags removes HTML tags from content for AI consumption.
// This is a simple approach — not a full sanitizer, just for truncation purposes.
func stripHTMLTags(s string) string {
	cleaned := htmlTagRegexp.ReplaceAllString(s, " ")
	// Collapse multiple whitespace into single space.
	spaceRegexp := regexp.MustCompile(`\s+`)
	return strings.TrimSpace(spaceRegexp.ReplaceAllString(cleaned, " "))
}

// stripThinkingContent removes <think>...</think> blocks from AI model responses.
// Some models (e.g. DeepSeek, QWen) include internal chain-of-thought reasoning in their output.
// This reasoning must be stripped to avoid leaking raw thinking content into user-visible summaries.
func stripThinkingContent(s string) string {
	return strings.TrimSpace(thinkingTagRegexp.ReplaceAllString(s, ""))
}

// truncateContent limits content to maxLen runes (not bytes) to control token usage.
// Using rune-based truncation avoids splitting multi-byte UTF-8 characters (e.g. Chinese),
// which would produce invalid/garbled strings and cause model errors.
func truncateContent(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

// chatMessage represents a single message in the chat completions request.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionRequest is the request body for the OpenAI-compatible chat completions endpoint.
type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

// chatCompletionResponse is the response from the OpenAI-compatible chat completions endpoint.
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}
