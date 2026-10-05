// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"bareproxy/internal/bp"
)

// The config change commands: plan, apply, rollback and history. Apart
// from plan --from, they ask the running server over its admin socket.
func init() {
	extraCommands["plan"] = planCmd
	extraCommands["apply"] = applyCmd
	extraCommands["rollback"] = rollbackCmd
	extraCommands["history"] = historyCmd
}

type changeFlags struct {
	pos                []string
	json, yes          bool
	from, plan, config string
}

func parseChangeFlags(args []string) (changeFlags, bool) {
	var f changeFlags
	for i := 0; i < len(args); i++ {
		next := func(dst *string) bool {
			i++
			if i >= len(args) {
				fmt.Fprintf(os.Stderr, "bareproxy: %s needs a value\n", args[i-1])
				return false
			}
			*dst = args[i]
			return true
		}
		ok := true
		switch a := args[i]; a {
		case "--json":
			f.json = true
		case "--yes", "-y":
			f.yes = true
		case "--from":
			ok = next(&f.from)
		case "--plan":
			ok = next(&f.plan)
		case "--config", "-c":
			ok = next(&f.config)
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "bareproxy: unknown option %s\n", a)
				return f, false
			}
			f.pos = append(f.pos, a)
		}
		if !ok {
			return f, false
		}
	}
	return f, len(f.pos) <= 1
}

func (f changeFlags) arg() string {
	if len(f.pos) == 1 {
		return f.pos[0]
	}
	return ""
}

// adminSocket finds the admin socket a config file names.
func adminSocket(file string) (string, error) {
	sock := "/run/bareproxy/admin.sock"
	if c, _ := bp.Load(file); c != nil {
		c.Close()
		sock = c.Admin
	}
	if sock == "off" {
		return "", fmt.Errorf("%s turns the admin socket off, so there is no running server to ask", file)
	}
	return sock, nil
}

// callAdmin sends one request to the admin socket.
func callAdmin(sock, method, path string, q url.Values, body string) (int, string, error) {
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}
	req, err := http.NewRequest(method, "http://bareproxy"+path+"?"+q.Encode(), strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return 0, "", fmt.Errorf("no BareProxy is running with the admin socket %s", sock)
	case errors.Is(err, syscall.EACCES):
		return 0, "", fmt.Errorf("no permission to use %s: run as root or as a member of its group", sock)
	case err != nil:
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// show prints an admin reply: errors go to stderr, except in JSON.
func show(code int, out string, asJSON bool) int {
	if code == http.StatusOK || asJSON {
		fmt.Print(out)
	} else {
		fmt.Fprint(os.Stderr, "bareproxy: "+out)
	}
	if code != http.StatusOK {
		return 1
	}
	return 0
}

func jsonQuery(asJSON bool) url.Values {
	q := url.Values{}
	if asJSON {
		q.Set("json", "1")
	}
	return q
}

func planCmd(args []string) int {
	f, ok := parseChangeFlags(args)
	if !ok {
		usage()
		return 2
	}
	file := configFile(f.arg())
	if f.from != "" {
		return offlinePlan(f.from, file, f.json)
	}
	text, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	sock, err := adminSocket(file)
	var code int
	var out string
	if err == nil {
		code, out, err = callAdmin(sock, "POST", "/plan", jsonQuery(f.json), string(text))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bareproxy: %v\nTo compare two files without a server: bareproxy plan FILE --from FILE\n", err)
		return 1
	}
	return show(code, out, f.json)
}

func offlinePlan(from, file string, asJSON bool) int {
	load := func(name string) *bp.Config {
		c, probs := bp.Load(name)
		for _, p := range probs {
			fmt.Fprintf(os.Stderr, "%s: %s\n", name, p)
		}
		if c == nil || bp.HasErrors(probs) {
			fmt.Fprintf(os.Stderr, "%s: has errors, so there is no plan\n", name)
			return nil
		}
		return c
	}
	old, cur := load(from), load(file)
	if old == nil || cur == nil {
		return 1
	}
	defer old.Close()
	defer cur.Close()
	p := bp.MakePlan(old, cur)
	if asJSON {
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		e.Encode(map[string]any{"plan_id": p.ID, "from": from, "file": file, "text": p.Text(), "plan": p})
		return 0
	}
	fmt.Printf("Plan %s, %s compared with %s:\n%s", p.ID, file, from, p.Text())
	return 0
}

func applyCmd(args []string) int {
	f, ok := parseChangeFlags(args)
	if !ok {
		usage()
		return 2
	}
	file := configFile(f.arg())
	text, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	sock, err := adminSocket(file)
	var code int
	var out string
	if err == nil {
		code, out, err = callAdmin(sock, "POST", "/plan", jsonQuery(true), string(text))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	var pr struct {
		PlanID    string `json:"plan_id"`
		Running   int    `json:"running"`
		Unchanged bool   `json:"unchanged"`
		Warnings  []string
		Text      string
		Error     string
	}
	if json.Unmarshal([]byte(out), &pr) != nil || code != http.StatusOK {
		if pr.Error != "" && !f.json {
			out = pr.Error + "\n"
		}
		return show(code, out, f.json)
	}
	planOut := os.Stdout
	if f.json {
		planOut = os.Stderr
	}
	if !f.json || !f.yes {
		fmt.Fprintf(planOut, "Plan %s, against running version %d:\n", pr.PlanID, pr.Running)
		if !pr.Unchanged {
			fmt.Fprint(planOut, pr.Text)
		}
		for _, w := range pr.Warnings {
			fmt.Fprintln(planOut, w)
		}
	}
	if pr.Unchanged {
		if f.json {
			fmt.Printf("{\"version\": %d, \"unchanged\": true}\n", pr.Running)
		} else {
			fmt.Printf("No changes: version %d keeps running.\n", pr.Running)
		}
		return 0
	}
	if !f.yes {
		if !isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, "bareproxy: nothing applied: without a terminal to ask in, apply needs --yes")
			return 1
		}
		fmt.Fprint(planOut, "Apply? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(planOut, "Nothing applied.")
			return 1
		}
	}
	q := jsonQuery(f.json)
	q.Set("plan", pr.PlanID)
	if f.plan != "" {
		q.Set("plan", f.plan)
	}
	code, out, err = callAdmin(sock, "POST", "/apply", q, string(text))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	return show(code, out, f.json)
}

// isTerminal reports whether f looks like a terminal: a character device
// other than /dev/null.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

func rollbackCmd(args []string) int {
	f, ok := parseChangeFlags(args)
	if !ok {
		usage()
		return 2
	}
	q := jsonQuery(f.json)
	if v := f.arg(); v != "" {
		q.Set("version", v)
	}
	return simpleAdmin(f, "POST", "/rollback", q)
}

func historyCmd(args []string) int {
	f, ok := parseChangeFlags(args)
	if !ok || len(f.pos) > 0 {
		usage()
		return 2
	}
	return simpleAdmin(f, "GET", "/history", jsonQuery(f.json))
}

func simpleAdmin(f changeFlags, method, path string, q url.Values) int {
	sock, err := adminSocket(configFile(f.config))
	var code int
	var out string
	if err == nil {
		code, out, err = callAdmin(sock, method, path, q, "")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bareproxy:", err)
		return 1
	}
	return show(code, out, f.json)
}
