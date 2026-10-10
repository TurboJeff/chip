.PHONY: build test dist clean

build:
	go build -o bin/ ./cmd/chip

test:
	go test ./...

# Cross-compile for the platforms owners are likely to run.
dist:
	@for p in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; ext=$$([ $$os = windows ] && echo .exe); \
		echo "chip-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" -o dist/chip-$$os-$$arch$$ext ./cmd/chip; \
	done

clean:
	rm -rf bin dist
