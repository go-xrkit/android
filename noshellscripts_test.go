// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptExtensions are the names that announce an interpreted script.
var scriptExtensions = []string{".sh", ".bash", ".zsh", ".py", ".pl", ".awk", ".rb"}

// scriptShebangs are the interpreters a script's first line names. They are
// read from the FILE, because a script does not need an extension to be one —
// and the thing being guarded against has no particular name.
var scriptShebangs = []string{
	"#!/bin/sh", "#!/bin/bash", "#!/bin/zsh", "#!/usr/bin/env sh",
	"#!/usr/bin/env bash", "#!/usr/bin/env zsh", "#!/usr/bin/env python",
	"#!/usr/bin/python", "#!/usr/bin/perl", "#!/usr/bin/env perl",
	"#!/usr/bin/awk", "#!/usr/bin/env ruby",
}

// TestNoShellScriptSitsInThisRepository.
//
// ⛔⛔ TWO WERE REMOVED ON THE SAME DAY, WHICH MAKES THIS A GATE AND NOT A NOTE.
// host/build.sh and host/pull-artifacts.sh were both replaced by Go in
// tools/xrpack and tools/xrrun on 2026-11-07, and in both cases the shell was
// not merely inelegant — it was WRONG in a way nothing could see:
//
//   - build.sh ended in `zip` then `zipalign`. zipalign aligns entries that are
//     STORED and `zip` deflates by default, so the payload was compressed, the
//     alignment check had no subject, and it printed "OK - compressed" and
//     exited 0. A gate with nothing to examine reports success.
//   - pull-artifacts.sh walked its destination up to the filesystem root
//     looking for a .git and refused if it found one — and then wrote to
//     "$dir/$f" with $f straight off the command line, so `../../x.png` landed
//     outside the directory it had just proved was safe.
//
// Both are the same shape: a check whose subject can leave the room. A Go
// program can say "this entry is compressed, so the question does not apply"
// and "this name is not a path element"; a pipeline answers with an empty
// string either way and the caller cannot tell which question it failed to ask.
//
// ⚠ THIS IS ABOUT FILES, NOT ABOUT THE SHELL. A `run:` block in a workflow is
// still a few commands in a row, and `go run ./tools/...` is invoked from one.
// What is refused is a SCRIPT: a committed file with logic in it that no test
// can reach and no compiler reads.
func TestNoShellScriptSitsInThisRepository(t *testing.T) {
	t.Parallel()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// ⛔ A GUARD MUST KNOW WHERE IT IS. A binary built with `go test -c` and run
	// elsewhere — which is how this suite's Linux lanes are exercised from a
	// macOS workstation — starts at "/" and would walk the whole filesystem.
	// See TestNoCompiledExecutableSitsInTheWorkTree, which learned this the
	// expensive way: seven minutes and a timeout.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("not running in the module's own directory (%s holds no go.mod), so there "+
			"is no work tree to check here; `go test ./...` in the repository is where "+
			"this guard is live", root)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "out":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if isScriptName(d.Name()) {
			found = append(found, rel+" (its name)")
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 3 {
			return nil //nolint:nilerr // an unreadable entry is not a finding
		}
		f, err := os.Open(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		defer func() { _ = f.Close() }()
		var head [64]byte
		n, _ := f.Read(head[:])
		if isScriptShebang(string(head[:n])) {
			found = append(found, rel+" (its shebang)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the work tree: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("interpreted script(s) in this repository: %v\n\n"+
			"⛔ EVERY TOOL HERE IS A GO PROGRAM WITH A TEST. The two scripts this "+
			"replaced were each wrong in a way that could not be seen: one ran an "+
			"alignment check on an entry the check did not apply to and reported OK, "+
			"the other walked its own guard past itself with a `..` in an argument. "+
			"Put the logic in a package under tools/ and drive it from a test.", found)
	}
}

// isScriptName reports whether a file name announces a script.
func isScriptName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, s := range scriptExtensions {
		if ext == s {
			return true
		}
	}
	return false
}

// isScriptShebang reports whether a file's first bytes name an interpreter.
func isScriptShebang(head string) bool {
	line := head
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	line = strings.TrimRight(line, " \t\r")
	for _, s := range scriptShebangs {
		if strings.HasPrefix(line, s) {
			return true
		}
	}
	return false
}

// TestTheScriptGuardRecognisesWhatItIsLookingFor.
//
// ⛔⛔ A GUARD THAT ASSERTS AN ABSENCE PASSES JUST AS WELL WHEN IT CANNOT SEE.
// The repository holds no script now, so the walk above is green whatever the
// two predicates do — including nothing at all. This is the control: it feeds
// them the names and the first lines of the files that WERE here until
// 2026-11-07, and the ones that must not be mistaken for them.
func TestTheScriptGuardRecognisesWhatItIsLookingFor(t *testing.T) {
	t.Parallel()
	t.Run("names", func(t *testing.T) {
		for _, c := range []struct {
			name string
			want bool
		}{
			{"build.sh", true},          // was host/build.sh
			{"pull-artifacts.sh", true}, // was host/pull-artifacts.sh
			{"deploy.bash", true},
			{"x.zsh", true},
			{"gen.py", true},
			{"munge.pl", true},
			{"count.awk", true},
			{"main.go", false},
			{"README.md", false},
			{"ci.yml", false},
			{"AndroidManifest.xml", false},
			{"debug.keystore", false},
			{"shellcheck", false}, // a name containing "sh" is not an extension
			{"x.shader", false},
			{"", false},
		} {
			if got := isScriptName(c.name); got != c.want {
				t.Errorf("isScriptName(%q) = %v, want %v", c.name, got, c.want)
			}
		}
	})

	t.Run("shebangs", func(t *testing.T) {
		for _, c := range []struct {
			head string
			want bool
		}{
			// The real first lines of the two files that were here.
			{"#!/bin/sh\n# Copyright (c) the go-xrkit/android authors.\n", true},
			{"#!/bin/sh\nset -eu\n", true},
			{"#!/usr/bin/env bash\n", true},
			{"#!/usr/bin/env python3\n", true},
			{"#!/bin/zsh", true},
			{"#!/usr/bin/perl -w\n", true},
			// And what must not be mistaken for one.
			{"// Copyright (c) the go-xrkit/android authors.\n", false},
			{"package android\n", false},
			{"# Build + deploy the Hugo site\n", false},
			{"<?xml version=\"1.0\"?>\n", false},
			{"#!/not/an/interpreter\n", false},
			{"", false},
			{"#", false},
		} {
			if got := isScriptShebang(c.head); got != c.want {
				t.Errorf("isScriptShebang(%q) = %v, want %v", c.head, got, c.want)
			}
		}
	})
}
