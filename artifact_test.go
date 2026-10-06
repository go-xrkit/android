// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// ⛔ A TRANSCRIPT LANDS WHERE run-as CAN READ IT. The app's private directory
// is the one the host hands over as HOME, and the external one — which a whole
// run was written to and lost — is hidden from the shell user. The rule is
// measured here rather than trusted, in both directions.
func TestSaveTranscriptWritesWhereTheHostSaysHomeIs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := android.SaveTranscript("screen.txt", "RESULT ok\n")
	if err != nil {
		t.Fatalf("SaveTranscript: %v", err)
	}
	if got, want := filepath.Dir(path), home; !sameDir(t, got, want) {
		t.Fatalf("the transcript landed in %q, want the host's HOME %q", got, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if string(b) != "RESULT ok\n" {
		t.Fatalf("the transcript holds %q", b)
	}
	// It is the person's own text on their own device: nobody else's account
	// has a reason to read it.
	//
	// ⛔ NOT ON WINDOWS, AND THE REASON IS NOT THAT IT IS AWKWARD THERE. Windows
	// has no POSIX permission bits: os.WriteFile's mode is dropped and the file
	// comes back 0666, so the assertion would be measuring the platform rather
	// than this package. The guarantee is about an Android device's
	// per-application directory, and the Windows lane exists to prove the
	// package still BUILDS where there is no host at all -- it has no such
	// directory to make a claim about.
	//
	// It is a BRANCH rather than a skip, and the other side asserts the
	// premise: the day Windows does honour the mode, this says so instead of
	// quietly passing on a condition that stopped being true.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	m := st.Mode().Perm()
	if runtime.GOOS == "windows" {
		if m&0o077 == 0 {
			t.Fatalf("the transcript is mode %04o on windows, where os.WriteFile's mode "+
				"is dropped and 0666 is expected; the reason this check is branched "+
				"around no longer holds", m)
		}
		return
	}
	if m&0o077 != 0 {
		t.Fatalf("the transcript is mode %04o, readable by other accounts", m)
	}
}

func TestSaveTranscriptFallsBackToTheArtifactDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv(android.ArtifactEnv, dir)
	path, err := android.SaveTranscript("glasses.txt", "ok")
	if err != nil {
		t.Fatalf("SaveTranscript: %v", err)
	}
	if got := filepath.Dir(path); !sameDir(t, got, dir) {
		t.Fatalf("the transcript landed in %q, want %q", got, dir)
	}
}

// Off a device with no HOME, the fallback is ArtifactDir — which REFUSES inside
// a work tree. A transcript must not get a pass the rule does not give.
func TestSaveTranscriptRefusesAWorkTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("making a work tree: %v", err)
	}
	t.Setenv("HOME", "")
	t.Setenv(android.ArtifactEnv, dir)
	if _, err := android.SaveTranscript("screen.txt", "ok"); !errors.Is(err, android.ErrInRepository) {
		t.Fatalf("SaveTranscript inside a work tree = %v, want ErrInRepository", err)
	}
}

func TestSaveTranscriptReportsADirectoryItCannotWriteTo(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-there")
	t.Setenv("HOME", home)
	if _, err := android.SaveTranscript("screen.txt", "ok"); err == nil {
		t.Fatal("SaveTranscript reported success writing into a directory that is not there")
	}
}

// sameDir compares two paths through EvalSymlinks: a temporary directory on
// darwin is under /var, which is a symlink to /private/var, so the two
// spellings are the same directory and a string comparison would fail on the
// platform this runs on.
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra == rb
}
