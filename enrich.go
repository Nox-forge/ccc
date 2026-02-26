package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	enrichURL     = "http://127.0.0.1:8094/enrich"
	enrichTimeout = 500 * time.Millisecond
	enrichMinLen  = 20
	enrichCooldown = 30 * time.Second
)

var (
	enrichMu       sync.Mutex
	enrichLastCall time.Time
)

type enrichResponse struct {
	FormattedContext string `json:"formatted_context"`
	EnrichmentMs    float64 `json:"enrichment_ms"`
	Memories        []any  `json:"memories"`
	MCPEntities     []any  `json:"mcp_entities"`
}

// enrichMessage calls the Memory Agent /enrich endpoint to add relevant context.
// Returns the original message with appended <memory-context> if relevant memories found.
// Fails silently — never blocks or breaks message delivery.
func enrichMessage(text string) string {
	// Skip short messages
	if len(text) < enrichMinLen {
		return text
	}

	// Skip commands
	if strings.HasPrefix(text, "/") {
		return text
	}

	// Skip common short responses
	lower := strings.ToLower(strings.TrimSpace(text))
	skipPatterns := []string{"ok", "yes", "no", "done", "thanks", "ty", "k", "yep", "nope", "sure", "lol", "haha"}
	for _, p := range skipPatterns {
		if lower == p {
			return text
		}
	}

	// Rate limit: one enrichment per cooldown period
	enrichMu.Lock()
	if time.Since(enrichLastCall) < enrichCooldown {
		enrichMu.Unlock()
		return text
	}
	enrichLastCall = time.Now()
	enrichMu.Unlock()

	// Call Memory Agent /enrich
	payload := fmt.Sprintf(`{"query": %q, "limit": 5, "threshold": 0.50}`, text)
	client := &http.Client{Timeout: enrichTimeout}
	resp, err := client.Post(enrichURL, "application/json", strings.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "enrich: request failed: %v\n", err)
		return text
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return text
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return text
	}

	var result enrichResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return text
	}

	// Only inject if there's meaningful context
	if result.FormattedContext == "" {
		return text
	}

	// Count results for logging
	nMem := len(result.Memories)
	nMcp := len(result.MCPEntities)
	fmt.Fprintf(os.Stderr, "enrich: %d memories + %d entities (%.0fms)\n", nMem, nMcp, result.EnrichmentMs)

	return text + "\n\n<memory-context>\nAutomatically retrieved relevant context:\n" + result.FormattedContext + "\n</memory-context>"
}

// getEnrichmentContext calls the Memory Agent /enrich endpoint and returns just the context string.
// Returns empty string if no relevant memories found or on any error.
func getEnrichmentContext(text string) string {
	if len(text) < enrichMinLen || strings.HasPrefix(text, "/") {
		return ""
	}

	lower := strings.ToLower(strings.TrimSpace(text))
	skipPatterns := []string{"ok", "yes", "no", "done", "thanks", "ty", "k", "yep", "nope", "sure", "lol", "haha"}
	for _, p := range skipPatterns {
		if lower == p {
			return ""
		}
	}

	payload := fmt.Sprintf(`{"query": %q, "limit": 5, "threshold": 0.50}`, text)
	client := &http.Client{Timeout: enrichTimeout}
	resp, err := client.Post(enrichURL, "application/json", strings.NewReader(payload))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return ""
	}

	var result enrichResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}

	if result.FormattedContext == "" {
		return ""
	}

	nMem := len(result.Memories)
	nMcp := len(result.MCPEntities)
	fmt.Fprintf(os.Stderr, "enrich-hook: %d memories + %d entities (%.0fms)\n", nMem, nMcp, result.EnrichmentMs)

	return "<memory-context>\nAutomatically retrieved relevant context:\n" + result.FormattedContext + "\n</memory-context>"
}
