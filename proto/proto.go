// Copyright 2026 Jeff Webb
// SPDX-License-Identifier: Apache-2.0

// Package proto holds the types and paths shared by the chipin server and chips.
package proto

import (
	"net/http"
	"net/url"
)

// Version is the chipin release version, reported by both binaries.
const Version = "0.1.0"

// StatusPath is served by a chip over the tunnel; the server polls it for health.
const StatusPath = "/_chip/status"

// ResourcePath is the path, on the chip side of the tunnel, that forwards
// apiPath (e.g. "/v1/chat/completions") to the named resource's upstream.
func ResourcePath(resource, apiPath string) string {
	return "/r/" + url.PathEscape(resource) + apiPath
}

// OpenAIPaths is the allowlist of OpenAI-compatible endpoints that may cross
// the tunnel, mapped to the only method allowed on each. Anything else (for
// example Ollama's /api/pull or /api/delete) is never reachable.
var OpenAIPaths = map[string]string{
	"/v1/chat/completions": http.MethodPost,
	"/v1/completions":      http.MethodPost,
	"/v1/embeddings":       http.MethodPost,
	"/v1/models":           http.MethodGet,
}

// Status is what a chip reports about itself and its shared resources.
type Status struct {
	Version   string           `json:"version"`
	OS        string           `json:"os"`
	Arch      string           `json:"arch"`
	CPUs      int              `json:"cpus"`
	Resources []ResourceStatus `json:"resources"`
}

type ResourceStatus struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Healthy       bool     `json:"healthy"`
	Error         string   `json:"error,omitempty"`
	Models        []string `json:"models"`
	MaxConcurrent int      `json:"max_concurrent"`
	// Loaded lists models currently in memory (warm). LoadedKnown is false
	// when the upstream can't say, in which case every model counts as warm.
	Loaded      []string `json:"loaded"`
	LoadedKnown bool     `json:"loaded_known"`
	InFlight    int64    `json:"in_flight"`
	LatencyMS   int64    `json:"latency_ms"`
}

// DefaultMaxConcurrent is how many requests a resource runs at once when its
// config doesn't say. chipin divides these slots between pools.
const DefaultMaxConcurrent = 2

// Usage is the OpenAI token usage block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
