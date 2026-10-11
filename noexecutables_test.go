// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// executableMagic are the first bytes of the executable formats this fleet
// builds. They are read from the FILE rather than inferred from its name,
// because the thing being guarded against has no particular name.
var executableMagic = [][]byte{
	{0x7f, 'E', 'L', 'F'},    // ELF: linux, android
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64-bit little-endian: darwin
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal
	{'M', 'Z'},               // PE: windows
}

// TestNoCompiledExecutableSitsInTheWorkTree.
//
// ⛔⛔ A `.gitignore` ENTRY IS THE WRONG FIX AND AN IGNORE RULE THAT NAMES A
// BINARY DIES AT THE RENAME. `go build ./cmd/xrusb` writes `xrusb` into the
// working directory, and on 2026-10-11 a 3.5 MB one sat there untracked, one
// `git add -A` from being committed — and this fleet has already tagged a
// release containing 8.4 MB of executables that way. Ignoring is a safety net,
// not a barrier: `git add -f`, a fresh clone, or any tool that does not consult
// it publishes the file anyway.
//
// So this reads the first bytes of every file and refuses the ones that are
// executables, which names no binary and survives every rename. The remedy it
// prints is the habit that prevents it: build with -o somewhere else.
//
// ⚠ IT WALKS, rather than listing one directory, because `go build ./...` from
// a subdirectory leaves them there instead.
func TestNoCompiledExecutableSitsInTheWorkTree(t *testing.T) {
	t.Parallel()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// ⛔⛔ AND IT MUST KNOW IT IS LOOKING AT THE MODULE. `go test` runs in the
	// package directory, but a binary built with `go test -c` and run elsewhere
	// — which is how this suite's linux lane is exercised from a macOS
	// workstation, inside a container — starts at "/" and would walk the whole
	// filesystem. It did: seven minutes, then a timeout, and every executable
	// in the image reported as a finding. A guard that cannot tell where it is
	// SAYS SO rather than walking whatever it was pointed at.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("not running in the module's own directory (%s holds no go.mod), so "+
			"there is no work tree to check here; `go test ./...` in the repository "+
			"is where this guard is live", root)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "out", "testdata":
				// .git holds packed objects that begin with anything at all;
				// host/out is where the APK build is told to put its own
				// products and is already ignored.
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 4 {
			return nil //nolint:nilerr // an unreadable entry is not a finding
		}
		f, err := os.Open(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		defer func() { _ = f.Close() }()
		var head [4]byte
		if _, err := f.Read(head[:]); err != nil {
			return nil //nolint:nilerr
		}
		for _, magic := range executableMagic {
			if bytes.HasPrefix(head[:], magic) {
				rel, _ := filepath.Rel(root, path)
				found = append(found, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the work tree: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("compiled executable(s) in the work tree: %v\n\n"+
			"⛔ ONE `git add -A` FROM BEING PUBLISHED, and a tag that catches one "+
			"freezes it forever. `go build ./cmd/x` writes into the working "+
			"directory; build with `-o /dev/null` to check that it compiles, or "+
			"`-o` somewhere outside the repository when you want to keep it.",
			found)
	}
}

// And the guard must be able to FAIL, or it is a line of reassurance rather
// than a control: the magic table is checked against a byte sequence that is not
// an executable and one that is.
func TestTheExecutableGuardRecognisesWhatItIsLookingFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		head []byte
		want bool
	}{
		{"an ELF binary", []byte{0x7f, 'E', 'L', 'F'}, true},
		{"a Mach-O binary", []byte{0xcf, 0xfa, 0xed, 0xfe}, true},
		{"a Windows binary", []byte{'M', 'Z', 0x90, 0x00}, true},
		{"Go source", []byte("// Co"), false},
		{"a Markdown file", []byte("# go-"), false},
		{"a PNG", []byte{0x89, 'P', 'N', 'G'}, false},
	} {
		got := false
		for _, magic := range executableMagic {
			if bytes.HasPrefix(c.head, magic) {
				got = true
				break
			}
		}
		if got != c.want {
			t.Errorf("%s: recognised as an executable = %v, want %v", c.name, got, c.want)
		}
	}
}
