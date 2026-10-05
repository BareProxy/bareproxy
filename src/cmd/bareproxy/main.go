// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Command bareproxy is BareProxy: a small web server and reverse proxy that
// explains every routing decision.
package main

import (
	"cmp"
	"context"
	"encoding/json"
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

// The options that several commands take.
const withConfig = "-c --config --json"
const withChange = withConfig + " -y --yes --from --plan"

// commands maps each command name to the function that runs it, the options
// it takes and how many plain arguments (min to max, -1 for any number). When
// file is set, the first plain argument is the config file.
var commands = map[string]struct {
	run      func(options) int
	opts     string
	file     bool
	min, max int
}{
	"run":      {run, "", true, 0, 1},
	"check":    {check, "", true, 0, 1},
	"explain":  {explain, "-c --config --offline -H", false, 2, 2},
	"why":      {why, withConfig, false, 1, 1},
	"plan":     {planCmd, withChange, true, 0, 1},
	"apply":    {applyCmd, withChange, true, 0, 1},
	"rollback": {rollbackCmd, withChange, false, 0, 1},
	"history":  {historyCmd, withChange, false, 0, 0},
	"tail":     {tailCmd, withConfig, false, 0, -1},
	"status":   {statusCmd, withConfig, false, 0, 0},
	"events":   {eventsCmd, withConfig, false, 0, 0},
}

func main() { os.Exit(execute(os.Args[1:])) }

// execute runs one command line and returns the exit code.
func execute(args []string) int {
	if len(args) == 0 {
		return badUsage("no command given")
	}
	switch args[0] {
	case "version", "--version":
		fmt.Printf("bareproxy %s, built with %s\n", bp.Version, runtime.Version())
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	}
	c, ok := commands[args[0]]
	if !ok {
		return badUsage(fmt.Sprintf("unknown command %q", args[0]))
	}
	o, err := parse(args[1:], c.opts)
	if err == nil && (len(o.pos) < c.min || c.max >= 0 && len(o.pos) > c.max) {
		err = errors.New("wrong number of arguments")
	}
	if err != nil {
		return badUsage(err.Error())
	}
	if c.file && len(o.pos) > 0 {
		o.config, o.pos = o.pos[0], o.pos[1:]
	}
	o.config = cmp.Or(o.config, os.Getenv("BAREPROXY_CONFIG"), "/etc/bareproxy/bareproxy.conf")
	return c.run(o)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  bareproxy run [FILE]                  start serving
  bareproxy check [FILE]                check a config without starting
  bareproxy explain [-c FILE] [--offline] [-H "Name: value"]... METHOD URL
                                        how a request is handled: by the running server, or by FILE with --offline or when none runs
  bareproxy why [-c FILE] [--json] ID   what happened to a request (memory first, then the trace log)
  bareproxy plan [FILE] [--from FILE]   what FILE would change, against the running config or another file
  bareproxy apply [FILE] [--yes] [--plan ID]
                                        check, show the plan, ask, make FILE live
  bareproxy rollback [-c FILE] [VERSION]
                                        make an earlier version live (default: the one before)
  bareproxy history [-c FILE]           config versions: time, how, Unix user, plan ID
  bareproxy tail [-c FILE] [--json] [FILTER]...
                                        records as they happen (times are UTC). Filters, all ANDed:
                                        'status>=500' status=404 pool=api site= host= outcome= method= path=/prefix
  bareproxy status [-c FILE] [--json]   listeners, sites, backends, certificates, recent error rates
  bareproxy events [-c FILE] [--json]   recent changes: backends up and down, certificates, reloads
  bareproxy version
FILE defaults to $BAREPROXY_CONFIG, then /etc/bareproxy/bareproxy.conf.
-c FILE (or --config FILE) names the config for every command but run and check, which take only FILE.
--json works with every command but run, check and explain.
`)
}

// badUsage says what is wrong with the command line, shows the usage and
// returns the exit code 2.
func badUsage(msg string) int {
	fmt.Fprintln(os.Stderr, "bareproxy: "+msg)
	usage()
	return 2
}

// fail says what went wrong and returns the exit code 1.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "bareproxy: "+format+"\n", a...)
	return 1
}

// options is what a command line holds. A command takes some of them.
type options struct {
	json, yes, offline bool
	config, from, plan string
	headers            []string // every -H
	pos                []string // the arguments that aren't options
}

// parse reads the options in names (the spelling a user types, such as
// "--config" and "-c", separated by spaces) from args, in any position. Other
// arguments go to pos. An option that isn't in names, or that lacks its
// value, is an error.
func parse(args []string, names string) (o options, err error) {
	flags := map[string]*bool{"--json": &o.json, "--yes": &o.yes, "-y": &o.yes, "--offline": &o.offline}
	values := map[string]*string{"--config": &o.config, "-c": &o.config, "--from": &o.from, "--plan": &o.plan}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !slices.Contains(strings.Fields(names), a):
			if strings.HasPrefix(a, "-") {
				return o, fmt.Errorf("unknown option %s", a)
			}
			o.pos = append(o.pos, a)
		case flags[a] != nil:
			*flags[a] = true
		case i == len(args)-1:
			return o, fmt.Errorf("%s needs a value", a)
		case a == "-H":
			i++
			o.headers = append(o.headers, args[i])
		default: // --config, -c, --from and --plan
			i++
			*values[a] = args[i]
		}
	}
	return o, nil
}

// settings loads a config just to learn where its admin socket and trace log
// are. A config with errors still has them, so only a file that can't be read
// is an error. Like loadChecked, it leaves the config's folders open until the
// command ends.
func settings(file string) (*bp.Config, error) {
	c, probs := bp.Load(file)
	if c == nil {
		return nil, fmt.Errorf("can't read the config: %s (--config FILE or $BAREPROXY_CONFIG says which one to use)", probs[0].Msg)
	}
	return c, nil
}

// loadChecked loads a config to use its rules. It reports the problems on
// stderr, and a config with errors is an error. The folders it opened stay
// open until the command ends.
func loadChecked(file string) (*bp.Config, error) {
	c, probs := bp.Load(file)
	for _, p := range probs {
		fmt.Fprintf(os.Stderr, "%s: %s\n", file, p)
	}
	if c == nil || bp.HasErrors(probs) {
		return nil, fmt.Errorf("%s has errors, so it can't be used", file)
	}
	return c, nil
}

// request sends one request to the admin socket that the config of o names.
// The target is the path with its query. A zero timeout means none, for
// streams. When nothing can be reached, the error says so in plain words.
func (o options) request(method, target, body string, timeout time.Duration) (*http.Response, error) {
	c, err := settings(o.config)
	if err != nil {
		return nil, err
	}
	if c.Admin == "off" {
		return nil, errors.New("the config turns the admin socket off, so there is no server to ask")
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Admin)
		if errors.Is(err, syscall.EACCES) {
			err = fmt.Errorf("no permission to use %s: run as root or as a member of its group", c.Admin)
		} else if err != nil {
			err = fmt.Errorf("no BareProxy is running on %s", c.Admin)
		}
		return conn, err
	}
	req, _ := http.NewRequest(method, "http://bareproxy"+target, strings.NewReader(body)) // the method and target are ours
	resp, err := (&http.Client{Timeout: timeout, Transport: &http.Transport{DialContext: dial}}).Do(req)
	return resp, errors.Unwrap(err) // Do wraps every error in a *url.Error, which only repeats the URL
}

// ask sends a request and reads the whole reply. Anything but a 200 is an
// error that carries the server's message, and the body comes back too. With
// --json it asks for JSON, so q must not be nil.
func (o options) ask(method, path string, q url.Values, body string) (string, error) {
	if o.json {
		q.Set("json", "1")
	}
	resp, err := o.request(method, path+"?"+q.Encode(), body, time.Minute)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil && resp.StatusCode != http.StatusOK {
		var e struct{ Error string } // a JSON reply says {"error": "..."}
		json.Unmarshal(b, &e)
		err = errors.New(cmp.Or(e.Error, strings.TrimSpace(string(b))))
	}
	return string(b), err
}

// show prints the reply that ask returned. Errors go to stderr, except with
// --json, where the server's reply is JSON too.
func (o options) show(out string, err error) int {
	switch {
	case err == nil:
		fmt.Print(out)
		return 0
	case o.json && out != "":
		fmt.Print(out)
		return 1
	}
	return fail("%v", err)
}

// toJSON renders v as indented JSON.
func toJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b) + "\n"
}

func run(o options) int {
	if err := bp.Run(o.config); err != nil {
		return fail("%v", err)
	}
	return 0
}

func check(o options) int {
	c, err := loadChecked(o.config)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("%s: ok, %s\n", o.config, bp.Summary(c))
	return 0
}

func explain(o options) int {
	method, rawURL := strings.ToUpper(o.pos[0]), o.pos[1]
	if !o.offline {
		// Ask the running server first: it knows the live backend states.
		q := url.Values{"method": {method}, "url": {rawURL}, "h": o.headers}
		if out, err := o.ask("GET", "/explain", q, ""); err == nil {
			fmt.Print(out)
			return 0
		}
	}
	c, err := loadChecked(o.config)
	if err != nil {
		return fail("%v", err)
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
	return o.show(out, err)
}

func why(o options) int {
	id := o.pos[0]
	rec := &bp.Record{}
	out, err := o.ask("GET", "/why", url.Values{"id": {id}}, "")
	if err == nil {
		err = json.Unmarshal([]byte(out), rec)
	} else {
		// The running server can't say (nothing answered, or it doesn't have
		// the request), so the trace log file is next.
		c, cerr := settings(o.config)
		switch {
		case cerr != nil:
			return fail("%v", cerr)
		case c.TraceLog == "stdout" || c.TraceLog == "off":
			return fail("%v, and the trace log goes to %s, so there is no file to look in", err, c.TraceLog)
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

// The config change commands: plan, apply, rollback and history. Apart from
// plan --from, they ask the running server over its admin socket.

func planCmd(o options) int {
	if o.from == "" {
		text, err := os.ReadFile(o.config)
		if err != nil {
			return fail("%v", err)
		}
		return o.show(o.ask("POST", "/plan", url.Values{}, string(text)))
	}
	// With --from, two files are compared and no server is asked.
	old, err1 := loadChecked(o.from)
	cur, err2 := loadChecked(o.config)
	if err := errors.Join(err1, err2); err != nil {
		return fail("%v", err)
	}
	p := bp.MakePlan(old, cur)
	if o.json {
		fmt.Print(toJSON(map[string]any{"plan_id": p.ID, "from": o.from, "file": o.config, "text": p.Text(), "plan": p}))
	} else {
		fmt.Printf("%s compared with %s:\n%s", o.config, o.from, p.Text())
	}
	return 0
}

func applyCmd(o options) int {
	text, err := os.ReadFile(o.config)
	if err != nil {
		return fail("%v", err)
	}
	out, err := options{config: o.config, json: true}.ask("POST", "/plan", url.Values{}, string(text))
	if err != nil {
		return o.show(out, err)
	}
	var pr struct {
		PlanID    string `json:"plan_id"`
		Running   int
		Unchanged bool
		Warnings  []string
		Text      string
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return fail("%v", err)
	}
	if o.plan != "" && o.plan != pr.PlanID {
		err := fmt.Errorf("plan %s doesn't match what %s would do now (that is plan %s), so nothing was applied; see the new plan with: bareproxy plan %[2]s", o.plan, o.config, pr.PlanID)
		return o.show(toJSON(map[string]string{"error": err.Error()}), err)
	}
	// The plan goes to stdout, or to stderr when stdout is for JSON. An
	// unchanged text has none: the server answers for it (it reloads the
	// certificate files, as SIGHUP does), and nothing needs a yes.
	planOut := map[bool]*os.File{false: os.Stdout, true: os.Stderr}[o.json]
	if !pr.Unchanged && (!o.json || !o.yes) {
		fmt.Fprintf(planOut, "Compared with running version %d:\n%s", pr.Running, pr.Text)
		for _, w := range pr.Warnings {
			fmt.Fprintln(planOut, w)
		}
	}
	if !o.yes && !pr.Unchanged {
		if !isTerminal(os.Stdin) {
			return fail("nothing applied: apply asks before it changes anything, and there is no terminal to ask in; add --yes to apply without asking")
		}
		fmt.Fprint(planOut, "Apply? [y/N] ")
		var answer string
		fmt.Fscanln(os.Stdin, &answer)
		if a := strings.ToLower(answer); a != "y" && a != "yes" {
			return fail("nothing applied")
		}
	}
	// pr.PlanID is the plan that was shown, and the one --plan named if it did.
	return o.show(o.ask("POST", "/apply", url.Values{"plan": {pr.PlanID}}, string(text)))
}

// isTerminal reports whether f looks like a terminal: a character device
// other than /dev/null.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	null, _ := os.Stat(os.DevNull)
	return err == nil && fi.Mode()&os.ModeCharDevice != 0 && !os.SameFile(fi, null)
}

func rollbackCmd(o options) int {
	return o.show(o.ask("POST", "/rollback", url.Values{"version": o.pos}, ""))
}

func historyCmd(o options) int { return o.show(o.ask("GET", "/history", url.Values{}, "")) }

// The trace commands: tail, status and events read what a running server
// knows.

func tailCmd(o options) int {
	for _, f := range o.pos {
		if _, err := bp.ParseTailFilter(f); err != nil {
			return badUsage(err.Error())
		}
	}
	resp, err := o.request("GET", "/tail?"+url.Values{"f": o.pos}.Encode(), "", 0)
	if err != nil {
		return fail("%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fail("%s", strings.TrimSpace(string(b)))
	}
	for dec := json.NewDecoder(resp.Body); ; {
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return fail("the server closed the connection")
		}
		if o.json {
			fmt.Printf("%s\n", raw)
			continue
		}
		var line struct {
			bp.Record
			Dropped int // a line that says {"dropped":N} instead of holding a record
		}
		json.Unmarshal(raw, &line)
		if line.ID == "" {
			fmt.Printf("-- %d records skipped, the reader was too slow --\n", line.Dropped)
		} else {
			fmt.Println(tailLine(&line.Record))
		}
	}
}

// tailLine is one record on one line: time (UTC), ID, method, host and path,
// status, outcome, time taken, and the rule that handled it.
func tailLine(r *bp.Record) string {
	_, t, _ := strings.Cut(r.Time, "T")
	status, rule := "-", "-"
	if r.Status != 0 {
		status = fmt.Sprint(r.Status)
	}
	if r.Line != 0 {
		rule = fmt.Sprintf("line %d: %s", r.Line, r.Rule)
	}
	return fmt.Sprintf("%s  %s  %-4s %s%s  %s  %s  %.1f ms  %s", strings.TrimSuffix(t, "Z"), r.ID, r.Method, r.Host, r.Path, status, r.Outcome, r.MS, rule)
}

// view shows what a running server reports at a path. With --json it prints
// the JSON as it came; otherwise it reads the JSON into a T and calls text.
func view[T any](o options, path string, text func(T)) int {
	var v T
	out, err := o.ask("GET", path, url.Values{}, "")
	if err != nil || o.json {
		return o.show(out, err)
	}
	if err = json.Unmarshal([]byte(out), &v); err != nil {
		return fail("%v", err)
	}
	text(v)
	return 0
}

func statusCmd(o options) int { return view(o, "/status", printStatus) }

func eventsCmd(o options) int { return view(o, "/events", printEvents) }

func printEvents(evs []bp.Event) {
	if len(evs) == 0 {
		fmt.Println("No events yet.")
	}
	for _, e := range evs {
		fmt.Printf("%s  %-11s  %s\n", e.Time, e.Kind, e.Text)
	}
}

func printStatus(st bp.Status) {
	fmt.Printf("BareProxy %s, up %s (since %s)\nConfig %s, version %d\n", st.Version, time.Duration(st.UptimeSeconds)*time.Second, st.Started, st.ConfigFile, st.ConfigVersion)
	if st.Mismatch != "" {
		fmt.Println("Warning: " + st.Mismatch)
	}
	fmt.Printf("\nListeners (%d)\n", len(st.Listeners))
	for _, l := range st.Listeners {
		fmt.Printf("  :%d %s  %s\n", l.Port, map[bool]string{false: "http", true: "https"}[l.TLS], strings.Join(l.Sites, ", "))
	}
	fmt.Printf("\nSites (%d)\n", len(st.Sites))
	for _, s := range st.Sites {
		fmt.Printf("  %s (line %d), rules: %d, %s\n", s.Name, s.Line, s.Rules, strings.Join(s.Addresses, " "))
	}
	fmt.Printf("\nPools (%d)\n", len(st.Pools))
	for _, p := range st.Pools {
		fmt.Printf("  %s (line %d): %d of %d up. Checks: %s\n", p.Name, p.Line, p.Up, p.Size, p.Checks)
		for _, b := range p.Backends {
			state := b.State
			if b.Reason != "" {
				state += " (" + b.Reason + ")"
			}
			fmt.Printf("    %-22s %s since %s, %d in flight, %d failures in a row\n", b.Addr, state, b.Since, b.InFlight, b.Failures)
		}
	}
	fmt.Printf("\nCertificates (%d)\n", len(st.Certificates))
	for _, c := range st.Certificates {
		fmt.Printf("  %s  %s  ends %s, %d days left\n", c.Site, c.Subject, c.NotAfter[:10], c.DaysLeft)
	}
	r := st.Requests
	if r.Ring.Limit == 0 {
		fmt.Println("\nNo requests counted: trace-memory is off, or none has arrived yet")
		return
	}
	fmt.Printf("\nRequests (the %d most recent, held in memory, back to %s)\n", r.Ring.Records, r.Ring.Oldest)
	rateLine("last minute", r.Last1m)
	rateLine("last 5 minutes", r.Last5m)
}

func rateLine(name string, r bp.Rate) {
	more := map[bool]string{false: " or more (the ring has wrapped)"}[r.Complete]
	fmt.Printf("  %-15s requests: %d%s, status 5xx: %d, proxy errors: %d\n", name, r.Requests, more, r.Status5xx, r.ProxyErrors)
}
