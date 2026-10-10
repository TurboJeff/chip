.PHONY: build test dist release clean

build:
	go build -o bin/ ./cmd/chip

test:
	go test ./...

# Cross-compile for the platforms owners are likely to run. The license
# files go with the binaries, as Apache-2.0 and MPL-2.0 require.
dist:
	@for p in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; ext=$$([ $$os = windows ] && echo .exe); \
		echo "chip-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" -o dist/chip-$$os-$$arch$$ext ./cmd/chip; \
	done
	cp LICENSE NOTICE THIRD_PARTY_NOTICES dist/

# Release archives for macOS and Linux. Each holds the chip binary and the
# license files; SHA256SUMS covers all of them. CI runs this on a v* tag.
VERSION ?= $(shell git describe --tags --always)
RELEASE_PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

release:
	rm -rf dist && mkdir -p dist
	@for p in $(RELEASE_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=chip_$(VERSION)_$${os}_$${arch}; \
		echo "$$name.tar.gz"; \
		mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" -o dist/$$name/chip ./cmd/chip || exit 1; \
		cp LICENSE NOTICE THIRD_PARTY_NOTICES README.md dist/$$name/; \
		tar -C dist -czf dist/$$name.tar.gz $$name; \
		rm -rf dist/$$name; \
	done
	cd dist && if command -v sha256sum >/dev/null; then sha256sum *.tar.gz; else shasum -a 256 *.tar.gz; fi > SHA256SUMS

clean:
	rm -rf bin dist
