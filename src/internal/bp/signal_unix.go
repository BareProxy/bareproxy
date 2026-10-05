// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package bp

import (
	"os"
	"syscall"
)

// reloadSignals are the signals that make BareProxy apply its config file.
var reloadSignals = []os.Signal{syscall.SIGHUP}

func isReloadSignal(s os.Signal) bool { return s == syscall.SIGHUP }
