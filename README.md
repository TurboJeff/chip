# chip

chip is the agent that shares a model server on your machine with a [chipin](https://chipin.dev) server. It opens an outbound connection to the server, so you don't open any ports, and your machine's address and upstream API keys never reach the people using it.

A resource is any OpenAI-compatible API: Ollama, llama.cpp server, vLLM, LM Studio, or a hosted provider.

## Run

Register a chip in the chipin dashboard and copy its token. Then, on the machine with the model server:

```sh
make build
./bin/chip -server https://hub.chipin.dev -token chip_xxx -upstream http://127.0.0.1:11434/v1 -name ollama
```

For more than one resource, or an upstream that needs a key, use a config file. See [`chip.example.json`](chip.example.json) and run `chip -config chip.json`. Upstream keys are read from the environment variable named in `api_key_env`, so they don't need to be in the file.

## What the chip allows

- Only `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings` and `GET /v1/models` are forwarded. Ollama's `/api/pull` and `/api/delete`, for example, can't be reached.
- Requests go only to upstreams in the chip's own config. The server can't choose the destination.
- Each resource runs at most `max_concurrent` requests at once (2 by default). The chip enforces this itself.
- Incoming credentials, cookies and forwarding headers are removed. The chip adds your upstream key, if you set one.
- Request bodies are limited to 32 MB.

The owner of a chip can see the prompts and responses that pass through it.

## Layout

```
cmd/chip   the binary
.          agent: reconnecting tunnel, path allowlist, upstream proxy
tunnel     WebSocket + yamux transport, used by both the chip and the server
proto      types and paths shared with the server
```

`make test` runs the tests. `make dist` builds binaries for macOS (arm64, amd64), Linux (amd64, arm64) and Windows (amd64).
