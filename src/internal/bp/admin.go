// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The admin socket's endpoints, besides explain. The config change ones
// (plan, apply, rollback, history) answer in plain text, or in JSON with
// json=1. The trace ones are in admin_trace.go.
func init() {
	adminHandlers = append(adminHandlers, func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /plan", s.adminPlan)
		mux.HandleFunc("POST /plan", s.adminPlan)
		mux.HandleFunc("POST /apply", s.adminApply)
		mux.HandleFunc("POST /rollback", s.adminRollback)
		mux.HandleFunc("GET /history", s.adminHistory)
		mux.HandleFunc("GET /why", s.adminWhy)
		mux.HandleFunc("GET /tail", s.adminTail)
		mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Status()) })
		mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Current().Mem.Events()) })
	})
}

type peerKey struct{}

// userName names a Unix user, or gives the number when there's no name.
func userName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return "uid " + strconv.Itoa(uid)
}

func peerUser(r *http.Request) string {
	u, _ := r.Context().Value(peerKey{}).(string)
	return u
}

// reply sends v as JSON with json=1, or text otherwise.
func reply(w http.ResponseWriter, r *http.Request, code int, v any, text string) {
	body, ctype := text, "text/plain; charset=utf-8"
	if r.URL.Query().Get("json") == "1" {
		js, _ := json.MarshalIndent(v, "", "  ")
		body, ctype = string(js)+"\n", "application/json"
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func replyErr(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusBadRequest
	var ce *ConfigError
	var pe *PlanChangedError
	if errors.As(err, &ce) {
		code = http.StatusUnprocessableEntity
	} else if errors.As(err, &pe) {
		code = http.StatusConflict
	}
	reply(w, r, code, map[string]string{"error": err.Error()}, err.Error()+"\n")
}

// changeText returns the config text a request carries: the file named by
// file=, or the body.
func changeText(r *http.Request) (string, error) {
	if f := r.URL.Query().Get("file"); f != "" {
		b, err := os.ReadFile(f)
		return string(b), err
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err == nil && len(b) == 0 {
		err = errors.New("give a config: file=PATH, or the text in the body")
	}
	return string(b), err
}

type planReply struct {
	PlanID    string      `json:"plan_id"`
	Running   int         `json:"running"`
	Unchanged bool        `json:"unchanged"`
	Warnings  []string    `json:"warnings,omitempty"`
	Text      string      `json:"text"`
	Plan      *PlanResult `json:"plan"`
}

func (s *Server) adminPlan(w http.ResponseWriter, r *http.Request) {
	text, err := changeText(r)
	if err != nil {
		replyErr(w, r, err)
		return
	}
	file, _ := filepath.Abs(s.file)
	c, probs := Parse(file, text)
	defer c.Close()
	if HasErrors(probs) {
		replyErr(w, r, &ConfigError{probs})
		return
	}
	s.mu.Lock()
	rt, unchanged := s.rt.Load(), text == s.cs.text
	s.mu.Unlock()
	p := MakePlan(rt.Cfg, c)
	p.OldVer = rt.Version
	out := planReply{PlanID: p.ID, Running: rt.Version, Unchanged: unchanged, Text: p.Text(), Plan: p}
	for _, pr := range probs {
		out.Warnings = append(out.Warnings, pr.String())
	}
	changes := out.Text
	if unchanged {
		changes = "No changes: the text is the running config.\n"
	}
	reply(w, r, http.StatusOK, out, fmt.Sprintf("Compared with running version %d:\n%s%s", rt.Version, changes, asLines(out.Warnings)))
}

// asLines joins texts as lines, each ending in a newline.
func asLines(texts []string) string {
	var b strings.Builder
	for _, t := range texts {
		b.WriteString(t + "\n")
	}
	return b.String()
}

func appliedText(a *Applied) string {
	if a.Unchanged {
		return asLines(a.Warnings) + fmt.Sprintf("No changes: version %d keeps running.\n", a.Version)
	}
	return asLines(a.Warnings) + fmt.Sprintf("Version %d is running (it was %d).\n", a.Version, a.Previous)
}

func (s *Server) adminApply(w http.ResponseWriter, r *http.Request) {
	text, err := changeText(r)
	if err != nil {
		replyErr(w, r, err)
		return
	}
	a, err := s.Apply(Change{Text: text, How: "apply", User: peerUser(r), PlanID: r.URL.Query().Get("plan")})
	if err != nil {
		replyErr(w, r, err)
		return
	}
	reply(w, r, http.StatusOK, a, appliedText(a))
}

func (s *Server) adminRollback(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("version")
	n, err := strconv.Atoi(v) // no version gives 0: the one before the running version
	if v != "" && (err != nil || n < 1) {
		replyErr(w, r, fmt.Errorf("bad version %q: give a version number from the history", v))
		return
	}
	a, err := s.Rollback(n, peerUser(r))
	if err != nil {
		replyErr(w, r, err)
		return
	}
	text := appliedText(a)
	if a.Plan != nil && !a.Unchanged {
		text = a.Plan.Text() + text
	}
	reply(w, r, http.StatusOK, a, text)
}

func (s *Server) adminHistory(w http.ResponseWriter, r *http.Request) {
	es := s.History()
	running := s.Current().Version
	if es == nil {
		reply(w, r, http.StatusOK, map[string]any{"running": running, "versions": []Entry{}},
			"No config history is kept: the state folder couldn't be used (see the log).\n")
		return
	}
	var b strings.Builder
	b.WriteString("Version  Time (UTC)           How       User        Plan\n")
	for _, e := range es {
		t, _ := time.Parse(time.RFC3339, e.Time)
		plan := e.Plan
		if e.From > 0 {
			plan = strings.TrimSpace(fmt.Sprintf("%s (version %d again)", plan, e.From))
		}
		if e.Version == running {
			plan += " (running)"
		}
		fmt.Fprintf(&b, "%7d  %s  %-8s  %-10s  %s\n", e.Version, t.UTC().Format("2006-01-02 15:04:05"), e.How, e.User, plan)
	}
	reply(w, r, http.StatusOK, map[string]any{"running": running, "versions": es}, b.String())
}
