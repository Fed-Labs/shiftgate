SHELL := /bin/sh

.PHONY: all build test test-race test-e2e test-stress test-desktop lint lint-ci fmt vet agent cli control desktop desktop-frontend desktop-rust dist clean

all: test build

build:
	mkdir -p bin
	go build -trimpath -o bin/shift ./cmd/shift
	go build -trimpath -o bin/shift-agent ./cmd/shift-agent
	go build -trimpath -o bin/shift-control ./cmd/shift-control

agent:
	go build -trimpath -o bin/shift-agent ./cmd/shift-agent

cli:
	go build -trimpath -o bin/shift ./cmd/shift

control:
	go build -trimpath -o bin/shift-control ./cmd/shift-control

test:
	go test ./...

test-race:
	go test -race ./...

# Real CRIU, real workloads, two-agent migrations. Skips (with the reason
# printed) when not run as root or without SHIFT_TEST_E2E=1.
test-e2e:
	SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestE2E -v -count=1

# The heavyweight suite: huge memory, 1500 processes, latency proxy, source
# death mid-transfer, disk exhaustion, OOM. Needs SHIFT_TEST_E2E too.
test-stress:
	SHIFT_TEST_E2E=1 SHIFT_TEST_STRESS=1 go test ./tests/integration/ -run TestStress -v -count=1 -timeout 45m

# The desktop client's agent transport: HTTP/1.1 over the agent Unix socket.
test-desktop:
	cargo test --manifest-path apps/desktop/src-tauri/Cargo.toml

fmt:
	gofmt -w $$(find cmd internal tests -name '*.go' -type f)

vet:
	go vet ./...

lint: fmt vet test

# Static analysis in CI: gofmt + vet + golangci-lint (config in
# .golangci.yml). Does not run tests; CI runs them separately.
lint-ci:
	@files=$$(gofmt -l $$(find cmd internal tests -name '*.go' -type f)); \
	if [ -n "$$files" ]; then echo "gofmt found unformatted files:"; echo "$$files"; exit 1; fi
	go vet ./...
	golangci-lint run ./...

# Desktop client (apps/desktop — Tauri + React). The frontend type-checks and
# bundles with tsc/vite; the Rust shell compiles with cargo. `desktop-bundle`
# produces installers via the Tauri bundler.
desktop: desktop-frontend desktop-rust

desktop-frontend:
	npm --prefix apps/desktop install
	npm --prefix apps/desktop run build

desktop-rust:
	cargo build --manifest-path apps/desktop/src-tauri/Cargo.toml

desktop-bundle:
	npm --prefix apps/desktop install
	npm --prefix apps/desktop run tauri build

# Release artifacts for the one-command installer. Asset names carry no
# version so a release URL's "latest/download" redirect always resolves;
# the binaries themselves answer --version and the update system verifies
# the version a release document signs.
dist: build
	mkdir -p dist
	tar -czf dist/shift-linux-amd64.tar.gz -C bin shift shift-agent shift-control
	sha256sum dist/shift-linux-amd64.tar.gz >dist/shift-linux-amd64.tar.gz.sha256
	printf 'shift %s (linux-amd64)\n' "$$(awk '/const Version/ {print $$4}' internal/config/config.go | tr -d '"')" \
		| tee dist/shift-linux-amd64.txt
	@echo 'publish the three files in dist/ as release assets; the installer fetches'
	@echo 'them from https://github.com/Fed-Labs/shiftgate/releases/latest/download/shift-linux-amd64.tar.gz'

clean:
	rm -rf bin dist apps/desktop/dist apps/desktop/node_modules apps/desktop/src-tauri/target

