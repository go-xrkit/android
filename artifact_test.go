// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-xrkit/android"
)

// ⛔⛔ THE RULE IS CHECKED IN BOTH DIRECTIONS, because a guard verified one way
// round is a guard that might be refusing everything. Inside a work tree it must
// FAIL and name the tree; outside it must create the directory and return it.
//
// It exists because a live test in this fleet once wrote a capture of a whole
// desktop into a public repository's testdata/, one `git add -A` from
// publication. A frame from a phone is a picture of whatever its owner was
// looking at.
func TestArtifactDirRefusesAWorkTreeAndNothingElse(t *testing.T) {
	t.Run("refuses the directory that holds .git", func(t *testing.T) {
		tree := t.TempDir()
		if err := os.Mkdir(filepath.Join(tree, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(android.ArtifactEnv, tree)
		switch _, err := android.ArtifactDir(); {
		case err == nil:
			t.Error("a directory holding .git was accepted")
		case !errors.Is(err, android.ErrInRepository):
			t.Errorf("err = %v, want ErrInRepository", err)
		case !strings.Contains(err.Error(), tree):
			t.Errorf("err = %q, which does not name the work tree it found", err)
		}
	})

	// ⛔ AND IT WALKS UP. A capture written three directories deep inside a
	// repository is as committable as one at its root, and this is the half a
	// check on the immediate directory would miss.
	t.Run("refuses a directory deep inside one", func(t *testing.T) {
		tree := t.TempDir()
		if err := os.Mkdir(filepath.Join(tree, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		deep := filepath.Join(tree, "a", "b", "c")
		if err := os.MkdirAll(deep, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(android.ArtifactEnv, deep)
		if _, err := android.ArtifactDir(); !errors.Is(err, android.ErrInRepository) {
			t.Errorf("err = %v, want ErrInRepository for a directory inside the tree", err)
		}
	})

	t.Run("accepts a directory outside any repository", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "made", "on", "demand")
		t.Setenv(android.ArtifactEnv, dir)
		got, err := android.ArtifactDir()
		if err != nil {
			t.Fatalf("ArtifactDir = %v", err)
		}
		// ⚠ The path comes back ABSOLUTE, because a caller writing into it may
		// have changed directory since -- and a relative artefact path is how a
		// file ends up somewhere nobody looks.
		if !filepath.IsAbs(got) {
			t.Errorf("ArtifactDir = %q, which is not absolute", got)
		}
		if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
			t.Errorf("ArtifactDir did not create %s: %v", got, err)
		}
	})

	// ⚠ WITHOUT THE VARIABLE IT IS THE WORKING DIRECTORY, which is what the
	// Android host sets to the application's own files directory. The test
	// moves there rather than trusting the default, so it measures the fallback
	// instead of wherever `go test` happened to run.
	t.Run("falls back to the working directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(android.ArtifactEnv, "")
		t.Chdir(dir)
		got, err := android.ArtifactDir()
		if err != nil {
			t.Fatalf("ArtifactDir = %v", err)
		}
		// Compared through EvalSymlinks: a temporary directory on darwin is
		// under /var, which is a symlink to /private/var, so the two spellings
		// are the same directory and a string comparison would fail on the
		// platform this runs on.
		want, _ := filepath.EvalSymlinks(dir)
		gotReal, _ := filepath.EvalSymlinks(got)
		if gotReal != want {
			t.Errorf("ArtifactDir = %q, want the working directory %q", gotReal, want)
		}
	})
}
