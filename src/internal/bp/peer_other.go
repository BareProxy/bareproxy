// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package bp

import (
	"context"
	"net"
)

// peerContext can't name the admin socket's user outside Linux yet.
func peerContext(ctx context.Context, _ net.Conn) context.Context { return ctx }
