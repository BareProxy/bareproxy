// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Command bareproxy is BareProxy: a small web server and reverse proxy that
// explains every routing decision.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"bareproxy/internal/bp"
)

const defaultConfig = "/etc/bareproxy/bareproxy.conf"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run":
		os.Exit(run(args))
	case "check":
		os.Exit(check(args))
	case "explain":
		os.Exit(explain(args))
	case "why":
		os.Exit(why(args))
	case "version", "--version":
		fmt.Printf("bareproxy %s, built with %s\n", bp.Version, runtime.Version())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "bareproxy: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  bareproxy run [FILE]                  start serving
  bareproxy check [FILE]                check a config without starting
  bareproxy explain [--config FILE] [--offline] [-H "Name: value"]... METHOD URL
                                        how a request would be handled
  bareproxy why [--config FILE] ID      what happened to a request
  bareproxy version
FILE defaults to $BAREPROXY_CONFIG, then /etc/bareproxy/bareproxy.conf.
`)
}

func configFile(given string) string {
	if given != "" {
		return given
	}
	if env := os.Getenv("BAREPROXY_CONFIG"); env != "" {
		return env
	}
	return defaultConfig
}

func run(args []string) int {
	if len(args) > 1 {
		usage()
		return 2
	}
	file := ""
	if len(args) == 1 {
		file = args[0]
	}
	if err := bp.Run(configFile(file)); err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	return 0
}

func check(args []string) int {
	if len(args) > 1 {
		usage()
		return 2
	}
	file := ""
	if len(args) == 1 {
		file = args[0]
	}
	file = configFile(file)
	c, probs := bp.Load(file)
	for _, p := range probs {
		fmt.Println(p)
	}
	if c == nil || bp.HasErrors(probs) {
		fmt.Printf("%s: has errors, so it can't be used\n", file)
		return 1
	}
	defer c.Close()
	fmt.Printf("%s: ok, %s\n", file, bp.Summary(c))
	return 0
}

func explain(args []string) int {
	var file string
	var hs, pos []string
	offline := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i++; i < len(args) {
				file = args[i]
			}
		case "--offline":
			offline = true
		case "-H":
			if i++; i < len(args) {
				hs = append(hs, args[i])
			}
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 2 {
		usage()
		return 2
	}
	method, rawURL := strings.ToUpper(pos[0]), pos[1]
	file = configFile(file)
	c, probs := bp.Load(file)
	if c == nil || bp.HasErrors(probs) {
		for _, p := range probs {
			fmt.Fprintln(os.Stderr, p)
		}
		return 1
	}
	defer c.Close()
	if !offline {
		if out, err := askServer(c.Admin, method, rawURL, hs); err == nil {
			fmt.Print(out)
			return 0
		}
	}
	h := http.Header{}
	for _, kv := range hs {
		k, v, _ := strings.Cut(kv, ":")
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	rt, err := bp.NewRuntime(c, nil, 0, nil, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	out, err := bp.Explain(rt, method, rawURL, h, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	fmt.Print(out)
	return 0
}

// askServer asks a running BareProxy, over its admin socket, to explain a
// request with its live config and backend states.
func askServer(sock, method, rawURL string, hs []string) (string, error) {
	if sock == "" || sock == "off" {
		return "", fmt.Errorf("no admin socket")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}
	q := url.Values{"method": {method}, "url": {rawURL}, "h": hs}
	resp, err := client.Get("http://bareproxy/explain?" + q.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func why(args []string) int {
	var file string
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i++; i < len(args) {
				file = args[i]
			}
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 1 {
		usage()
		return 2
	}
	c, probs := bp.Load(configFile(file))
	if c == nil {
		for _, p := range probs {
			fmt.Fprintln(os.Stderr, p)
		}
		return 1
	}
	c.Close()
	if c.TraceLog == "stdout" || c.TraceLog == "off" {
		fmt.Fprintf(os.Stderr, "bareproxy: why reads the trace log, and this config sends it to %s; point trace-log at a file\n", c.TraceLog)
		return 1
	}
	rec, err := bp.FindRecord(c.TraceLog, pos[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	fmt.Print(bp.RenderWhy(rec))
	return 0
}
