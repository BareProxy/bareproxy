// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	names := []string{"--json", "-y", "-c", "--config", "-H"}
	cases := []struct {
		what   string
		args   []string
		strict bool
		want   options
		ok     bool
	}{
		{"options in any position", []string{"GET", "-H", "A: 1", "http://x/", "--json", "-H", "B: 2"}, false,
			options{json: true, headers: []string{"A: 1", "B: 2"}, pos: []string{"GET", "http://x/"}}, true},
		{"short and long spellings, the last one wins", []string{"-c", "a.conf", "--config", "b.conf", "-y"}, false,
			options{config: "b.conf", yes: true}, true},
		{"lenient: an unknown option is an argument", []string{"--bogus", "x"}, false,
			options{pos: []string{"--bogus", "x"}}, true},
		{"lenient: an option of another command is an argument", []string{"--offline"}, false,
			options{pos: []string{"--offline"}}, true},
		{"lenient: an option without its value is ignored", []string{"x", "--config"}, false,
			options{pos: []string{"x"}}, true},
		{"strict: an unknown option is refused", []string{"x", "--bogus"}, true, options{pos: []string{"x"}}, false},
		{"strict: an option of another command is refused", []string{"--offline"}, true, options{}, false},
		{"strict: an option without its value is refused", []string{"x", "--config"}, true, options{pos: []string{"x"}}, false},
		{"strict: a value may start with a dash", []string{"-c", "-odd.conf"}, true, options{config: "-odd.conf"}, true},
	}
	for _, c := range cases {
		got, ok := parse(c.args, c.strict, names...)
		if ok != c.ok || (ok && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("%s: parse(%q) = %+v, %v; want %+v, %v", c.what, c.args, got, ok, c.want, c.ok)
		}
	}
}
