// Command chip shares local resources (today: OpenAI-compatible AI APIs) with
// a chipin server over an outbound tunnel.
//
// Quick start, sharing a local Ollama:
//
//	chip -server https://chipin.example.com -token chip_xxx -upstream http://127.0.0.1:11434/v1
//
// Or with a config file (see chip.example.json):
//
//	chip -config chip.json
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TurboJeff/chip"
	"github.com/TurboJeff/chip/proto"
)

func main() {
	var (
		configPath    = flag.String("config", "", "path to a JSON config file")
		server        = flag.String("server", "", "chipin server URL (overrides config)")
		token         = flag.String("token", os.Getenv("CHIP_TOKEN"), "chip token (or $CHIP_TOKEN; overrides config)")
		upstream      = flag.String("upstream", "", "share one OpenAI-compatible API at this base URL, e.g. http://127.0.0.1:11434/v1")
		name          = flag.String("name", "ai", "resource name when using -upstream")
		apiKeyEnv     = flag.String("api-key-env", "", "env var holding the upstream API key when using -upstream")
		maxConcurrent = flag.Int("max-concurrent", 2, "max simultaneous requests when using -upstream")
		version       = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *version {
		fmt.Println("chip", proto.Version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := &chip.Config{}
	if *configPath != "" {
		c, err := chip.LoadConfig(*configPath)
		if err != nil {
			fatal(log, err)
		}
		cfg = c
	}
	if *server != "" {
		cfg.Server = *server
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *upstream != "" {
		cfg.Resources = append(cfg.Resources, chip.Resource{
			Name: *name, Kind: "openai", Upstream: *upstream, APIKeyEnv: *apiKeyEnv, MaxConcurrent: *maxConcurrent,
		})
	}

	agent, err := chip.New(cfg, log)
	if err != nil {
		fatal(log, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.Run(ctx); err != nil {
		fatal(log, err)
	}
}

func fatal(log *slog.Logger, err error) {
	log.Error(err.Error())
	os.Exit(1)
}
