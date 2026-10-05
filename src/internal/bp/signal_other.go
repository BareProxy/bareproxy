// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package bp

import "os"

// reloadSignals is empty where there is no SIGHUP (Windows, WebAssembly):
// the config is applied with bareproxy apply instead.
var reloadSignals []os.Signal

func isReloadSignal(os.Signal) bool { return false }
