# Contributing to SHIFT

This repository implements a computation-mobility platform under one rule that
governs every change: **never claim behavior the code does not have.** The
spec forbids TODOs, placeholders, mocked core functionality, and
"implement later" sections — the same bar applies to contributions.

## Setup

Prerequisites: Go 1.24+, Linux (the agent is Linux-only by design), CRIU 4+
for any checkpoint/restore work, and GNU tar. The desktop client additionally
needs Node 20+, npm, and a Rust toolchain.

```bash
git clone <repository> && cd shift
make build          # shift, shift-agent, shift-control into ./bin
make test           # unit tests
make vet
```

## The rules that matter

1. **No fake implementations.** A component that cannot be implemented
   honestly for an environment fails loudly there (`unavailableEngine`
   reports "CRIU is not installed"; the doctor command surfaces it). Never
   substitute a stub that returns success.
2. **Never claim a successful migration unless the state was actually
   restored.** The migration state machine persists each stage before the
   next begins; failure at any stage rolls back and preserves the source.
3. **No invented cryptography.** Everything crypto-shaped lives in
   `internal/securestore` and `internal/checkpoint/repository.go` (manifest
   signing) using standard-library primitives. New needs go there, not into
   ad-hoc code.
4. **Secrets are never logged.** Audit any log line your change adds.
5. **Filesystem capture stays explicit.** Only paths attached to a workload
   are captured. There is no whole-machine snapshot.

## Code style

- Standard library first. The runtime dependencies are `pgx` (PostgreSQL)
  and `golang.org/x/crypto` — anything else needs a convincing reason and a
  pinned version.
- Structured logging (`log/slog`) only; no `fmt.Println` outside docs and
  CLI output.
- Errors wrap with `%w` and carry context; `errorlint` is enabled.
- `gofmt` clean, `go vet` clean, `golangci-lint` clean (`make lint-ci`).
- Go source comments explain *why*, not *what*.

## Testing expectations

- New behavior ships with tests in the same package (`*_test.go`).
- Tests that need external services (PostgreSQL, CRIU, interpreters) skip
  with an explicit reason when unavailable — they never fake a pass.
- Changes to checkpoint, restore, or migration paths should run the
  end-to-end suite locally (see [Development](development.md)); the CI e2e
  lane fails when nothing actually ran.

## Committing

- Write commit messages that explain the why. Keep commits buildable.
- Do not commit binaries, `bin/`, `node_modules/`, or `target/`.
- The default branch for PRs is `main`.

## Review checklist for reviewers

- [ ] No TODO/FIXME/placeholder in the diff.
- [ ] Failure paths fail loudly and preserve the source workload.
- [ ] New log lines carry no secrets or workload plaintext.
- [ ] Tests skip honestly rather than passing vacuously.
- [ ] Docs under `docs/` updated when behavior or interfaces change.
