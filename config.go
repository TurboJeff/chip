// Copyright 2026 Jeff Webb
// SPDX-License-Identifier: Apache-2.0

package chip

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"

	"github.com/TurboJeff/chip/proto"
)

// Config is a chip's configuration. The donor decides what is shared; the
// server can only reach the resources listed here, through the allowlisted
// OpenAI endpoints.
type Config struct {
	Server    string     `json:"server"`
	Token     string     `json:"token"`
	Resources []Resource `json:"resources"`
}

type Resource struct {
	// Name identifies the resource within this chip (letters, digits, - and _).
	Name string `json:"name"`
	// Kind is the resource type. Only "openai" (an OpenAI-compatible API) exists today.
	Kind string `json:"kind"`
	// Upstream is the base URL of the API including /v1, e.g. http://127.0.0.1:11434/v1.
	Upstream string `json:"upstream"`
	// APIKey, or the env var named by APIKeyEnv, is added to upstream requests.
	// It never leaves this machine.
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Models, if set, limits which upstream models are shared.
	Models []string `json:"models,omitempty"`
	// MaxConcurrent caps simultaneous requests (default 2). chipin shares
	// these slots between the pools the chip chips in to.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) Validate() error {
	if c.Server == "" {
		return fmt.Errorf("server is required")
	}
	// The token is sent when the chip connects, so it must not cross the
	// network unencrypted. Plain http is allowed only for a local hub.
	su, err := url.Parse(c.Server)
	if err != nil || su.Host == "" || (su.Scheme != "https" && !(su.Scheme == "http" && isLocal(su.Hostname()))) {
		return fmt.Errorf("server must be an https URL (http is allowed only for localhost)")
	}
	if c.Token == "" {
		return fmt.Errorf("token is required")
	}
	if len(c.Resources) == 0 {
		return fmt.Errorf("at least one resource is required")
	}
	seen := map[string]bool{}
	for i := range c.Resources {
		r := &c.Resources[i]
		if !nameRE.MatchString(r.Name) {
			return fmt.Errorf("resource name %q must match %s", r.Name, nameRE)
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate resource name %q", r.Name)
		}
		seen[r.Name] = true
		if r.Kind == "" {
			r.Kind = "openai"
		}
		if r.MaxConcurrent <= 0 {
			r.MaxConcurrent = proto.DefaultMaxConcurrent
		}
		if r.Kind != "openai" {
			return fmt.Errorf("resource %q: unsupported kind %q", r.Name, r.Kind)
		}
		u, err := url.Parse(r.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("resource %q: upstream must be an http(s) URL", r.Name)
		}
		if r.APIKey == "" && r.APIKeyEnv != "" {
			r.APIKey = os.Getenv(r.APIKeyEnv)
			if r.APIKey == "" {
				return fmt.Errorf("resource %q: env var %s is empty", r.Name, r.APIKeyEnv)
			}
		}
	}
	return nil
}
