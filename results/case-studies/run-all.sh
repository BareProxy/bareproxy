#!/usr/bin/env bash
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# run-all.sh: reruns every case study and the use-examples recipes and
# rewrites their logs next to the scripts. Needs bash, python3 with the
# cryptography package, curl and jq; about six minutes. BP=/path/to/bareproxy
# tests another build; by default the v0.16.0 Linux amd64 release is
# downloaded and checked against its SHA256SUMS.
set -e
cd "$(dirname "$0")"
for s in inbox-deploy inbox-crash inbox-retries-replay drift-watch use-examples; do
	echo "running $s"
	./$s.sh >$s.log 2>&1
done
