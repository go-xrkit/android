// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ArtifactEnv names the directory artefacts are written to, overriding the
// default.
const ArtifactEnv = "XRKIT_ARTIFACT_DIR"

// ErrInRepository is reported when the chosen artefact directory is inside a
// git work tree.
var ErrInRepository = errors.New("a capture must never be written where it can be committed")

// ArtifactDir is where a command may write what it captured, created if it is
// not there.
//
// ⛔ NEVER INSIDE A GIT WORK TREE, and that is not a convention -- it is the
// mistake this fleet has already made: a live test wrote a capture of a whole
// desktop into a public repository's testdata/, one `git add -A` from
// publication. A frame from a phone is a picture of whatever its owner was
// looking at, which is the same thing.
//
// A .gitignore entry would be the wrong fix: ignoring is a safety net, not a
// barrier -- `git add -f`, a fresh clone, or any tool that does not consult it
// publishes the file anyway. This REFUSES, including when a person points
// ArtifactEnv at their own work tree.
//
// ⚠ IT LIVES IN THE PACKAGE RATHER THAN IN ONE COMMAND, because it was in one
// command and a second command needed it. Two copies of a rule like this is how
// one of them quietly stops refusing.
// The two calls that can fail only when the working directory has been taken
// away underneath the process, replaced in tests.
//
// ⛔ THEY ARE SEAMS BECAUSE THE REAL FAILURE IS UNREACHABLE HERE. A test that
// removes its own working directory is SKIPPED on darwin, which still answers
// Getwd for a directory that is gone -- and a skipped test covers nothing while
// looking like it does. The same seam shape as photo.go's writeFile.
var (
	getwd = os.Getwd
	abs   = filepath.Abs
)

func ArtifactDir() (string, error) {
	dir := os.Getenv(ArtifactEnv)
	if dir == "" {
		// On a device the host sets the working directory to the app's external
		// files directory, which is inside no repository and survives the run.
		wd, err := getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	absDir, err := abs(dir)
	if err != nil {
		return "", err
	}
	for d := absDir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return "", fmt.Errorf("%w: %s is inside the work tree at %s", ErrInRepository, absDir, d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return absDir, os.MkdirAll(absDir, 0o755)
}
