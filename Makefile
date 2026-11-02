SHELL := /bin/sh

.PHONY: all build test test-race test-e2e test-stress test-desktop lint lint-ci fmt vet agent cli control desktop desktop-frontend desktop-rust desktop-bundle dist clean

all: test build

# CGO_ENABLED=0 ships static binaries that run on musl and older-glibc
# hosts; -buildvcs=false keeps bytes identical across checkouts of the same
# tree. The release tarball is assembled with fixed metadata below so its
# published .sha256 can be reproduced from the same sources.
GO_BUILD := CGO_ENABLED=0 go build -trimpath -buildvcs=false

build:
	mkdir -p bin
	$(GO_BUILD) -o bin/shiftgate ./cmd/shift
	$(GO_BUILD) -o bin/shift-agent ./cmd/shift-agent
	$(GO_BUILD) -o bin/shift-control ./cmd/shift-control
	$(GO_BUILD) -o bin/shift-gc ./cmd/shift-gc
	$(GO_BUILD) -o bin/shift-verify ./cmd/shift-verify
	$(GO_BUILD) -o bin/shift-relay ./cmd/shift-relay

agent:
	$(GO_BUILD) -o bin/shift-agent ./cmd/shift-agent

cli:
	$(GO_BUILD) -o bin/shiftgate ./cmd/shift

control:
	$(GO_BUILD) -o bin/shift-control ./cmd/shift-control

test:
	go test ./...

test-race:
	go test -race ./...

# Real CRIU, real workloads, two-agent migrations. Skips (with the reason
# printed) when not run as root or without SHIFT_TEST_E2E=1 — and fails if
# every test skips, so a lane with zero coverage never reports green.
# -timeout matches CI's 30m: the 18 sequential real-CRIU scenarios exceed
# go test's 10m default.
test-e2e:
	SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestE2E -v -count=1 -timeout 30m

# The heavyweight suite: huge memory, 1500 processes, latency proxy, source
# death mid-transfer, disk exhaustion, OOM. Needs SHIFT_TEST_E2E too.
test-stress:
	SHIFT_TEST_E2E=1 SHIFT_TEST_STRESS=1 go test ./tests/integration/ -run TestStress -v -count=1 -timeout 45m

# The desktop client's agent transport: HTTP/1.1 over the agent Unix socket.
test-desktop:
	cargo test --manifest-path apps/desktop/src-tauri/Cargo.toml

fmt:
	gofmt -w $$(find cmd internal tests database -name '*.go' -type f)

vet:
	go vet ./...

lint: fmt vet test

# Static analysis in CI: gofmt + vet + golangci-lint (config in
# .golangci.yml). Does not run tests; CI runs them separately.
lint-ci:
	@files=$$(gofmt -l $$(find cmd internal tests database -name '*.go' -type f)); \
	if [ -n "$$files" ]; then echo "gofmt found unformatted files:"; echo "$$files"; exit 1; fi
	go vet ./...
	golangci-lint run ./...

# Desktop client (apps/desktop — Tauri + React). The frontend type-checks and
# bundles with tsc/vite; the Rust shell compiles with cargo. `desktop-bundle`
# produces installers via the Tauri bundler.
desktop: desktop-frontend desktop-rust

desktop-frontend:
	npm --prefix apps/desktop ci
	npm --prefix apps/desktop run build

desktop-rust:
	cargo build --manifest-path apps/desktop/src-tauri/Cargo.toml

desktop-bundle:
	npm --prefix apps/desktop ci
	npm --prefix apps/desktop run tauri build

# Release artifacts for the one-command installer. Asset names carry no
# version so a release URL's "latest/download" redirect always resolves;
# the binaries themselves answer `shiftgate version` and the update system
# verifies the version a release document signs. The tarball carries every
# shipped binary: the CLI, the agent, the control plane, the offline GC, the
# encrypted-state verifier (backup-verify.sh needs it), and the relay.
dist: build
	mkdir -p dist
	tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner -C bin -cf - shiftgate shift-agent shift-control shift-gc shift-verify shift-relay | gzip -n > dist/shiftgate-linux-amd64.tar.gz
	cd dist && sha256sum shiftgate-linux-amd64.tar.gz >shiftgate-linux-amd64.tar.gz.sha256
	printf 'shiftgate %s (linux-amd64)\n' "$$(awk '/const Version/ {print $$4}' internal/config/config.go | tr -d '"')" \
		| tee dist/shiftgate-linux-amd64.txt
	@echo 'publish the files in dist/ as release assets; the installer fetches'
	@echo 'them from https://github.com/Fed-Labs/shiftgate/releases/latest/download/shiftgate-linux-amd64.tar.gz'

clean:
	rm -rf bin dist apps/desktop/dist apps/desktop/node_modules apps/desktop/src-tauri/target

