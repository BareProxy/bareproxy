// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

// Command planref prints what bp.MakePlan says about two config files, run
// natively. check.mjs compares its output with what the browser demo shows
// for the same two configs. With -nodisk the files are read the way the demo
// reads them (no folders, no certificates); without it, the way the server
// does.
//
//	usage: planref [-nodisk] OLD NEW
package main

import (
	"flag"
	"fmt"
	"os"

	"bareproxy/internal/bp"
)

func main() {
	nodisk := flag.Bool("nodisk", false, "read configs without opening folders or certificates, as the browser demo does")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: planref [-nodisk] OLD NEW")
		os.Exit(2)
	}
	oldC := load(flag.Arg(0), *nodisk)
	newC := load(flag.Arg(1), *nodisk)
	defer oldC.Close()
	defer newC.Close()
	fmt.Print(bp.MakePlan(oldC, newC).Text())
}

func load(file string, nodisk bool) *bp.Config {
	var c *bp.Config
	var probs []bp.Problem
	if nodisk {
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "planref:", err)
			os.Exit(1)
		}
		c, probs = bp.ParseWith("bareproxy.conf", string(data), bp.ParseOptions{NoDisk: true})
	} else {
		c, probs = bp.Load(file)
	}
	if c == nil || bp.HasErrors(probs) {
		for _, p := range probs {
			fmt.Fprintln(os.Stderr, file+":", p)
		}
		os.Exit(1)
	}
	return c
}
