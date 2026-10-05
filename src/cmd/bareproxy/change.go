// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"bareproxy/internal/bp"
)

// The config change commands: plan, apply, rollback and history. Apart
// from plan --from, they ask the running server over its admin socket.

// parseChangeFlags reads the options the four commands share. They take at
// most one argument, which arg returns.
func parseChangeFlags(args []string) (o options, ok bool) {
	o, ok = parse(args, true, "--json", "--yes", "-y", "--from", "--plan", "--config", "-c")
	return o, ok && len(o.pos) <= 1
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

// callServer sends a request to the server that a config file names.
func callServer(file, method, path string, q url.Values, body string) (int, string, error) {
	sock, err := adminSocket(file)
	if err != nil {
		return 0, "", err
	}
	return callAdmin(sock, method, path, q, body)
}

// show prints an admin reply: errors go to stderr, except in JSON.
func show(code int, out string, asJSON bool) int {
	switch {
	case code == http.StatusOK:
		fmt.Print(out)
		return 0
	case asJSON:
		fmt.Print(out)
	default:
		fmt.Fprint(os.Stderr, "bareproxy: "+out)
	}
	return 1
}

// jsonOr renders v as indented JSON, or returns text as it is.
func jsonOr(asJSON bool, v any, text string) string {
	if !asJSON {
		return text
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b) + "\n"
}

func jsonQuery(asJSON bool) url.Values {
	if asJSON {
		return url.Values{"json": {"1"}}
	}
	return url.Values{}
}

func planCmd(args []string) int {
	f, ok := parseChangeFlags(args)
	if !ok {
		return badUsage()
	}
	file := configFile(f.arg())
	if f.from != "" {
		return offlinePlan(f.from, file, f.json)
	}
	text, err := os.ReadFile(file)
	if err != nil {
		return fail("%v", err)
	}
	code, out, err := callServer(file, "POST", "/plan", jsonQuery(f.json), string(text))
	if err != nil {
		return fail("%v\nTo compare two files without a server: bareproxy plan FILE --from FILE", err)
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
	text := p.Text()
	fmt.Print(jsonOr(asJSON,
		map[string]any{"plan_id": p.ID, "from": from, "file": file, "text": text, "plan": p},
		fmt.Sprintf("%s compared with %s:\n%s", file, from, text)))
	return 0
}

func applyCmd(args []string) int {
	f, ok := parseChangeFlags(args)
	if !ok {
		return badUsage()
	}
	file := configFile(f.arg())
	text, err := os.ReadFile(file)
	if err != nil {
		return fail("%v", err)
	}
	sock, err := adminSocket(file)
	if err != nil {
		return fail("%v", err)
	}
	code, out, err := callAdmin(sock, "POST", "/plan", jsonQuery(true), string(text))
	if err != nil {
		return fail("%v", err)
	}
	var pr struct {
		PlanID      string `json:"plan_id"`
		Running     int
		Unchanged   bool
		Warnings    []string
		Text, Error string
	}
	if json.Unmarshal([]byte(out), &pr) != nil || code != http.StatusOK {
		if pr.Error != "" && !f.json {
			out = pr.Error + "\n"
		}
		return show(code, out, f.json)
	}
	if f.plan != "" && f.plan != pr.PlanID {
		msg := (&bp.PlanChangedError{ID: f.plan}).Error()
		return show(http.StatusConflict, jsonOr(f.json, map[string]string{"error": msg}, msg+"\n"), f.json)
	}
	// The plan goes to stdout, or to stderr when stdout is for JSON.
	planOut := os.Stdout
	if f.json {
		planOut = os.Stderr
	}
	if !f.json || !f.yes {
		fmt.Fprintf(planOut, "Compared with running version %d:\n", pr.Running)
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
			return fail("nothing applied: without a terminal to ask in, apply needs --yes")
		}
		fmt.Fprint(planOut, "Apply? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(planOut, "Nothing applied.")
			return 1
		}
	}
	// pr.PlanID is the plan that was shown, and the one --plan named if it did.
	q := jsonQuery(f.json)
	q.Set("plan", pr.PlanID)
	code, out, err = callAdmin(sock, "POST", "/apply", q, string(text))
	if err != nil {
		return fail("%v", err)
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
		return badUsage()
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
		return badUsage()
	}
	return simpleAdmin(f, "GET", "/history", jsonQuery(f.json))
}

func simpleAdmin(f options, method, path string, q url.Values) int {
	code, out, err := callServer(configFile(f.config), method, path, q, "")
	if err != nil {
		return fail("%v", err)
	}
	return show(code, out, f.json)
}
