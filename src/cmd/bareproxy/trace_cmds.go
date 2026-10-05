// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bareproxy/internal/bp"
)

// traceClient makes an HTTP client that talks to the admin socket. A zero
// timeout means none, for streams.
func traceClient(sock string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}
}

// traceGet asks the admin socket for a path and returns the answer.
func traceGet(sock, path string) ([]byte, int, error) {
	if sock == "" || sock == "off" {
		return nil, 0, fmt.Errorf("the config has no admin socket")
	}
	resp, err := traceClient(sock, 5*time.Second).Get("http://bareproxy" + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// traceWhy asks a running BareProxy for a request it still has in memory.
// It returns no record and no error when the server isn't there or doesn't
// have the request, so the caller can try the trace log file; asked says
// whether a server answered at all.
func traceWhy(sock, prefix string) (rec *bp.Record, asked bool, err error) {
	b, code, err := traceGet(sock, "/why?id="+url.QueryEscape(prefix))
	switch {
	case err != nil:
		return nil, false, nil
	case code == http.StatusNotFound:
		return nil, true, nil
	case code != http.StatusOK:
		return nil, true, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	rec = &bp.Record{}
	if err := json.Unmarshal(b, rec); err != nil {
		return nil, true, err
	}
	return rec, true, nil
}

// traceJSON prints a value as one JSON line.
func traceJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}
