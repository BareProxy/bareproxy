#!/bin/sh
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# go test for CI: runs go test with the given arguments, and when it fails,
# repeats the failing tests' lines as one error annotation, so the reason is
# readable from the run's summary without the full log.
#
#   usage (from src/): sh ../live/ci-test.sh [go test arguments]
set -u
out=$(mktemp)
go test "$@" >"$out" 2>&1
code=$?
cat "$out"
if [ "$code" -ne 0 ]; then
  msg=$(grep -E -e '--- FAIL|^FAIL|_test\.go:[0-9]+|panic:|cannot|undefined' "$out" | head -n 60 |
    awk '{ gsub(/%/, "%25"); gsub(/\r/, "%0D"); printf "%s%%0A", $0 }')
  echo "::error title=go test failed on $(go env GOOS)/$(go env GOARCH)::$msg"
fi
rm -f "$out"
exit "$code"
