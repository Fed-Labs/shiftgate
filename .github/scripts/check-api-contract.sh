#!/usr/bin/env bash
# Spec-vs-route contract check: every route the control plane registers must
# appear in the OpenAPI document, and every documented path must be served.
# Backed by `shift-verify api-contract`, whose parsers are unit-tested.
set -euo pipefail
cd "$(dirname "$0")/../.."
exec go run ./cmd/shift-verify api-contract \
  --spec api/openapi/shift.yaml \
  --routes internal/controlplane/server.go
