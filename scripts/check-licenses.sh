#!/bin/sh
# check-licenses.sh — open-core split guard (plan 3 BR-01). Fails when a file
# under an ELv2 directory lacks the stub, when the ELv2 license text is
# missing, or when a new top-level directory is unmapped.
set -eu

cd "$(dirname "$0")/.."

fail=0

# 1. The ELv2 text must exist and be the real license (not a placeholder).
for f in LICENSE-ELv2 LICENSE.elastic.txt; do
  if ! grep -q "Elastic License 2.0" "$f" 2>/dev/null; then
    echo "MISSING OR INVALID: $f (must contain the Elastic License 2.0 text)" >&2
    fail=1
  fi
done

# 2. Every ELv2 directory must carry its LICENSE stub.
for d in cmd/shift-control cmd/shift-relay apps/web internal/controlplane \
         internal/billing internal/identity internal/scheduler internal/relay \
         internal/database; do
  if [ ! -f "$d/LICENSE" ]; then
    echo "MISSING STUB: $d/LICENSE (ELv2 directory without its license stub)" >&2
    fail=1
  elif ! grep -q "Elastic License 2.0" "$d/LICENSE"; then
    echo "BAD STUB: $d/LICENSE does not name the Elastic License 2.0" >&2
    fail=1
  fi
done

# 3. Apache-side stubs where confusion is possible.
for d in integrations apps/desktop; do
  if [ ! -f "$d/LICENSE" ]; then
    echo "MISSING STUB: $d/LICENSE (Apache directory without its NOTICE reference)" >&2
    fail=1
  fi
done

# 4. Every top-level directory with source files must be mentioned in the
#    NOTICE license map — a new unmapped directory is a split violation.
for d in */; do
  d="${d%/}"
  case "$d" in
     .git|.github|.shift|node_modules|dist|target|testdata|tmp|bin) continue ;;

  esac
  if find "$d" -name '*.go' -o -name '*.ts' -o -name '*.tsx' -o -name '*.rs' 2>/dev/null | head -1 | grep -q .; then
    if ! grep -q "$d/" NOTICE; then
      echo "UNMAPPED DIRECTORY: $d/ is not named in the NOTICE license map" >&2
      fail=1
    fi
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "license split check FAILED" >&2
  exit 1
fi
echo "license split check ok"
