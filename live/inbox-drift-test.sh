#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# inbox-drift-test.sh: runs the tests of the webhook inbox and the DNS and
# TLS drift watcher and writes what they show to results/inbox-test.log and
# results/drift-test.log. The site's numbers about the two come from these
# files.
#
# Inbox: every test of internal/inbox and the server's inbox tests, then the
# crash test (BareProxy killed with SIGKILL three times while deliveries are
# in flight, 300 webhooks a run) KILL_RUNS times, default 5, with the totals.
# Drift: every test of internal/drift, the server's and the command's drift
# tests, all on a stub resolver and local TLS servers, so no internet.
#
# Needs Go 1.27 (GO=path to use another go). Run from anywhere:
#   live/inbox-drift-test.sh
set -euo pipefail
cd "$(dirname "$0")/../src"
GO=${GO:-go}
KILL_RUNS=${KILL_RUNS:-5}
export GOTOOLCHAIN=local GOFLAGS=-mod=vendor GOPROXY=off
res=../results
head() {
	echo "# $1, $(date -u +%Y-%m-%d), $("$GO" version | cut -d' ' -f3-4), $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //' || uname -m), $(nproc 2>/dev/null || echo '?') CPUs"
}
run() { echo "\$ go test $*"; "$GO" test "$@" 2>&1 | grep -vE '^=== (RUN|PAUSE|CONT)' | grep -v 'http: TLS handshake error'; }

{
	head "webhook inbox tests"
	run -count=1 -v ./internal/inbox/
	run -count=1 -v -run 'TestInbox' ./internal/bp/
	echo
	echo "## The crash test, $KILL_RUNS runs"
	out=$("$GO" test -count="$KILL_RUNS" -v -run 'TestInboxSurvivesSIGKILL$' ./internal/bp/ 2>&1)
	echo "$out" | grep -E 'SIGKILLs during delivery|--- (PASS|FAIL)|^(ok|FAIL)'
	echo "$out" | grep 'SIGKILLs during delivery' | awk '{
		ev += $2; k += $4; d += $8; lost += $10; dup += $12; again += $16 }
		END { printf "Total: %d webhooks, %d SIGKILLs during delivery, %d deliveries: %d lost, %d delivered twice unmarked, %d sent again marked in-doubt\n", ev, k, d, lost, dup, again }'
} >"$res/inbox-test.log"

{
	head "drift watcher tests"
	run -count=1 -v ./internal/drift/
	run -count=1 -v -run 'Drift' ./internal/bp/ ./cmd/bareproxy/
} >"$res/drift-test.log"

grep -E '^(ok|FAIL)|^Total' "$res/inbox-test.log" "$res/drift-test.log"
! grep -q -E '^(FAIL|--- FAIL)' "$res/inbox-test.log" "$res/drift-test.log"
