// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//go:build js && wasm

// Command bareproxy-wasm is BareProxy's browser demo. It compiles the same
// parser, matcher, explain and plan the server uses to WebAssembly and
// exposes three functions to the page: bareproxyCheck, bareproxyExplain and
// bareproxyPlan. Nothing leaves the browser. There is no disk in a browser,
// so folders and certificate files are not read (see bp.ParseOptions).
package main

import (
	"fmt"
	"net/http"
	"strings"
	"syscall/js"

	"bareproxy/internal/bp"
)

// The name shown wherever a config's file name is printed.
const confName = "bareproxy.conf"

func main() {
	js.Global().Set("bareproxyVersion", bp.Version)
	js.Global().Set("bareproxyCheck", js.FuncOf(guard(check)))
	js.Global().Set("bareproxyExplain", js.FuncOf(guard(explain)))
	js.Global().Set("bareproxyPlan", js.FuncOf(guard(plan)))
	select {}
}

// guard turns a panic into an error result, so one bad input can't stop the
// Go runtime and leave the page without its functions.
func guard(f func(args []js.Value) any) func(js.Value, []js.Value) any {
	return func(_ js.Value, args []js.Value) (out any) {
		defer func() {
			if r := recover(); r != nil {
				out = "bareproxy: internal error: " + fmt.Sprint(r) + "\n"
			}
		}()
		return f(args)
	}
}

func arg(args []js.Value, i int) string {
	if i < len(args) && args[i].Type() == js.TypeString {
		return args[i].String()
	}
	return ""
}

func parse(text string) (*bp.Config, []bp.Problem) {
	return bp.ParseWith(confName, text, bp.ParseOptions{NoDisk: true})
}

// check returns {ok, problems: [{line, msg, warn}], summary}. A config
// without errors gets the warnings bareproxy check gives, plan's included.
func check(args []js.Value) any {
	c, probs := parse(arg(args, 0))
	ok := c != nil && !bp.HasErrors(probs)
	if ok {
		probs = bp.Warnings(c)
	}
	list := make([]any, 0, len(probs))
	for _, p := range probs {
		list = append(list, map[string]any{"line": p.Line, "msg": p.Msg, "warn": p.Warn})
	}
	summary := "has errors, so it can't be used"
	if ok {
		summary = bp.Summary(c)
		c.Close()
	}
	return map[string]any{"ok": ok, "problems": list, "summary": summary}
}

// problemText prints problems the way the command line does.
func problemText(probs []bp.Problem) string {
	var b strings.Builder
	for _, p := range probs {
		fmt.Fprintln(&b, p)
	}
	return b.String()
}

// explain returns the text bareproxy explain --offline prints for the
// config, or the config's problems when it can't be used.
func explain(args []js.Value) any {
	c, probs := parse(arg(args, 0))
	if c == nil || bp.HasErrors(probs) {
		return "The config has errors, so explain can't use it:\n" + problemText(probs)
	}
	defer c.Close()
	h := http.Header{}
	for _, line := range strings.Split(arg(args, 3), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	rt, err := bp.NewRuntime(c, nil, 0, nil, false)
	if err != nil {
		return "bareproxy: " + err.Error() + "\n"
	}
	out, err := bp.Explain(rt, strings.ToUpper(strings.TrimSpace(arg(args, 1))), strings.TrimSpace(arg(args, 2)), h, false)
	if err != nil {
		return "bareproxy: " + err.Error() + "\n"
	}
	return out
}

// plan returns what replacing the first config with the second would change.
func plan(args []js.Value) any {
	oldC, oldProbs := parse(arg(args, 0))
	newC, newProbs := parse(arg(args, 1))
	switch {
	case oldC == nil || bp.HasErrors(oldProbs):
		return "The running config (section 1) has errors, so there is nothing to compare with:\n" + problemText(oldProbs)
	case newC == nil || bp.HasErrors(newProbs):
		return "The new config has errors, so it can't be planned:\n" + problemText(newProbs)
	}
	defer oldC.Close()
	defer newC.Close()
	return bp.MakePlan(oldC, newC).Text()
}
