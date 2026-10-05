// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Command bareproxy is BareProxy: a small web server and reverse proxy that
// explains every routing decision.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"bareproxy/internal/bp"
)

const defaultConfig = "/etc/bareproxy/bareproxy.conf"

// commands maps each command name to the function that runs it. Each
// returns the exit code.
var commands = map[string]func(args []string) int{
	"run":      run,
	"check":    check,
	"explain":  explain,
	"why":      why,
	"plan":     planCmd,
	"apply":    applyCmd,
	"rollback": rollbackCmd,
	"history":  historyCmd,
	"tail":     tailCmd,
	"status":   statusCmd,
	"events":   eventsCmd,
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(badUsage())
	}
	cmd, args := os.Args[1], os.Args[2:]
	if f, ok := commands[cmd]; ok {
		os.Exit(f(args))
	}
	switch cmd {
	case "version", "--version":
		fmt.Printf("bareproxy %s, built with %s\n", bp.Version, runtime.Version())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "bareproxy: unknown command %q\n", cmd)
		os.Exit(badUsage())
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  bareproxy run [FILE]                  start serving
  bareproxy check [FILE]                check a config without starting
  bareproxy explain [--config FILE] [--offline] [-H "Name: value"]... METHOD URL
                                        how a request would be handled
  bareproxy why [--config FILE] [--json] ID
                                        what happened to a request (memory first, then the trace log)
  bareproxy plan [FILE] [--from FILE]   what FILE would change, against the running config or another file
  bareproxy apply [FILE] [--yes] [--plan ID]
                                        check, show the plan, ask, make FILE live
  bareproxy rollback [VERSION]          make an earlier version live (default: the one before)
  bareproxy history                     config versions: time, how, Unix user, plan ID
  bareproxy tail [--config FILE] [--json] [FILTER]...
                                        records as they happen (times are UTC). Filters, all ANDed:
                                        'status>=500' status=404 pool=api site= host= outcome= method= path=/prefix
  bareproxy status [--config FILE] [--json]
                                        listeners, sites, backends, certificates, recent error rates
  bareproxy events [--config FILE] [--json]
                                        recent changes: backends up and down, certificates, reloads
  bareproxy version
FILE defaults to $BAREPROXY_CONFIG, then /etc/bareproxy/bareproxy.conf.
plan, apply, rollback and history take --json; rollback and history take --config FILE.
`)
}

// badUsage shows the usage and returns the exit code for a command line
// that doesn't fit.
func badUsage() int {
	usage()
	return 2
}

// fail says what went wrong and returns the exit code 1.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "bareproxy: "+format+"\n", a...)
	return 1
}

func printProblems(w io.Writer, probs []bp.Problem) {
	for _, p := range probs {
		fmt.Fprintln(w, p)
	}
}

func configFile(given string) string {
	return cmp.Or(given, os.Getenv("BAREPROXY_CONFIG"), defaultConfig)
}

// options is what a command line holds. A command takes some of them.
type options struct {
	json, yes, offline bool
	config, from, plan string
	headers            []string // every -H
	pos                []string // the arguments that aren't options
}

// arg is the one argument that a command with at most one takes, or "".
func (o options) arg() string { return cmp.Or(o.pos...) }

// parse reads the options in names (with the spelling the user types, such
// as "--config" and "-c") from args, in any position. Other arguments go to
// pos. Lenient commands keep an unknown option as a plain argument and
// ignore an option that lacks its value. A strict command refuses both, says
// why and returns false.
func parse(args []string, strict bool, names ...string) (o options, ok bool) {
	flags := map[string]*bool{"--json": &o.json, "--yes": &o.yes, "-y": &o.yes, "--offline": &o.offline}
	values := map[string]*string{"--config": &o.config, "-c": &o.config, "--from": &o.from, "--plan": &o.plan}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !slices.Contains(names, a):
			if strict && strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "bareproxy: unknown option %s\n", a)
				return o, false
			}
			o.pos = append(o.pos, a)
		case flags[a] != nil:
			*flags[a] = true
		case i == len(args)-1: // an option that takes a value, and none is left
			if strict {
				fmt.Fprintf(os.Stderr, "bareproxy: %s needs a value\n", a)
				return o, false
			}
		case a == "-H":
			i++
			o.headers = append(o.headers, args[i])
		default: // --config, -c, --from and --plan
			i++
			*values[a] = args[i]
		}
	}
	return o, true
}

// loadConfig loads a config just to learn where its admin socket and trace
// log are. A config with errors still has them, so only a file that can't be
// read at all is reported.
func loadConfig(file string) *bp.Config {
	c, probs := bp.Load(configFile(file))
	if c == nil {
		printProblems(os.Stderr, probs)
		return nil
	}
	c.Close()
	return c
}

func run(args []string) int {
	if len(args) > 1 {
		return badUsage()
	}
	if err := bp.Run(configFile(cmp.Or(args...))); err != nil {
		return fail("%v", err)
	}
	return 0
}

func check(args []string) int {
	if len(args) > 1 {
		return badUsage()
	}
	file := configFile(cmp.Or(args...))
	c, probs := bp.Load(file)
	printProblems(os.Stdout, probs)
	if c == nil || bp.HasErrors(probs) {
		fmt.Printf("%s: has errors, so it can't be used\n", file)
		return 1
	}
	defer c.Close()
	fmt.Printf("%s: ok, %s\n", file, bp.Summary(c))
	return 0
}

func explain(args []string) int {
	o, _ := parse(args, false, "--config", "-c", "--offline", "-H")
	if len(o.pos) != 2 {
		return badUsage()
	}
	method, rawURL := strings.ToUpper(o.pos[0]), o.pos[1]
	c, probs := bp.Load(configFile(o.config))
	if c == nil || bp.HasErrors(probs) {
		printProblems(os.Stderr, probs)
		return 1
	}
	defer c.Close()
	if !o.offline {
		// Ask the running server first: it knows the live backend states.
		q := url.Values{"method": {method}, "url": {rawURL}, "h": o.headers}
		if code, out, err := get(c.Admin, "/explain?"+q.Encode()); err == nil && code == http.StatusOK {
			fmt.Print(out)
			return 0
		}
	}
	h := http.Header{}
	for _, kv := range o.headers {
		k, v, _ := strings.Cut(kv, ":")
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	out := ""
	rt, err := bp.NewRuntime(c, nil, 0, nil, false)
	if err == nil {
		out, err = bp.Explain(rt, method, rawURL, h, false)
	}
	if err != nil {
		return fail("%v", err)
	}
	fmt.Print(out)
	return 0
}

func why(args []string) int {
	o, _ := parse(args, false, traceOptions...)
	if len(o.pos) != 1 {
		return badUsage()
	}
	c := loadConfig(o.config)
	if c == nil {
		return 1
	}
	id := o.pos[0]
	rec, asked, err := traceWhy(c.Admin, id)
	if rec == nil && err == nil {
		if c.TraceLog == "stdout" || c.TraceLog == "off" {
			if asked {
				return fail("no request with an ID starting %s in memory, and this config sends the trace log to %s, so there is no file to look in", id, c.TraceLog)
			}
			return fail("no running server answered, and this config sends the trace log to %s; point trace-log at a file", c.TraceLog)
		}
		rec, err = bp.FindRecord(c.TraceLog, id)
	}
	if err != nil {
		return fail("%v", err)
	}
	if o.json {
		b, _ := bp.RecordJSON(rec)
		fmt.Println(string(b))
	} else {
		fmt.Print(bp.RenderWhy(rec))
	}
	return 0
}

// adminClient makes an HTTP client that talks to the admin socket. A zero
// timeout means none, for streams.
func adminClient(sock string, timeout time.Duration) *http.Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{DialContext: dial}}
}

// adminCall sends one request to the admin socket. target is the path with
// its query. It returns the status code and the body.
func adminCall(sock, method, target, body string, timeout time.Duration) (int, string, error) {
	req, err := http.NewRequest(method, "http://bareproxy"+target, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	resp, err := adminClient(sock, timeout).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// get asks the admin socket for a path, for the commands that show what a
// running server knows.
func get(sock, target string) (int, string, error) {
	if sock == "" || sock == "off" {
		return 0, "", errors.New("the config has no admin socket")
	}
	return adminCall(sock, "GET", target, "", 5*time.Second)
}

// callAdmin sends one request to the admin socket. When nothing answers it
// says why in plain words.
func callAdmin(sock, method, path string, q url.Values, body string) (int, string, error) {
	code, out, err := adminCall(sock, method, path+"?"+q.Encode(), body, time.Minute)
	switch {
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		err = fmt.Errorf("no BareProxy is running with the admin socket %s", sock)
	case errors.Is(err, syscall.EACCES):
		err = fmt.Errorf("no permission to use %s: run as root or as a member of its group", sock)
	}
	return code, out, err
}
