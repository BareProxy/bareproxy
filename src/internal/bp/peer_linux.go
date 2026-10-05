// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package bp

import (
	"context"
	"net"
	"syscall"
)

// peerContext notes which Unix user is on the other end of an admin socket
// connection, from the kernel (SO_PEERCRED), for the history.
func peerContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ctx
	}
	var cred *syscall.Ucred
	raw.Control(func(fd uintptr) {
		cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cred == nil {
		return ctx
	}
	return context.WithValue(ctx, peerKey{}, userName(int(cred.Uid)))
}
