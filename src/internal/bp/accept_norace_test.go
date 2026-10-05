// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package bp

// accRace is true when the tests run under the race detector, which makes
// every request several times slower.
const accRace = false
