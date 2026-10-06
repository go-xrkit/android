// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ⚠ THE TWO ERROR PATHS, THROUGH THE SEAMS, because the real failure cannot be
// produced on the platform this is developed on: a test that removes its own
// working directory is SKIPPED on darwin, which still answers Getwd for a
// directory that is gone. A skipped test covers nothing while looking as though
// it does, which is worse than no test -- so the calls are replaceable and the
// failure is injected.
//
// They are worth covering rather than deleting: a program whose working
// directory has been taken away is exactly the one that must not decide for
// itself where to write a capture.
func TestArtifactDirReportsWhatItCannotFindOut(t *testing.T) {
	boom := errors.New("the working directory is gone")

	t.Run("it cannot ask where it is", func(t *testing.T) {
		t.Setenv(ArtifactEnv, "")
		old := getwd
		getwd = func() (string, error) { return "", boom }
		t.Cleanup(func() { getwd = old })

		switch _, err := ArtifactDir(); {
		case err == nil:
			t.Error("ArtifactDir answered although it could not find out where it is")
		case !errors.Is(err, boom):
			t.Errorf("err = %v, want the failure it was given", err)
		}
	})

	// ⛔ A SEPARATE RETURN FOR A SEPARATE QUESTION: where am I, and what does
	// this name mean from here. A relative name is resolved against the working
	// directory, so it fails for the same cause one step later.
	t.Run("it cannot make the name absolute", func(t *testing.T) {
		t.Setenv(ArtifactEnv, "shots")
		old := abs
		abs = func(string) (string, error) { return "", boom }
		t.Cleanup(func() { abs = old })

		switch _, err := ArtifactDir(); {
		case err == nil:
			t.Error("a name resolved although it could not be made absolute")
		case !errors.Is(err, boom):
			t.Errorf("err = %v, want the failure it was given", err)
		}
	})

	// ⚠ AND THE SEAMS ARE THE REAL CALLS BY DEFAULT, which is the half that
	// stops this from testing a fiction: with nothing replaced, the package
	// still answers from the actual filesystem.
	t.Run("unreplaced, they are os.Getwd and filepath.Abs", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(ArtifactEnv, "")
		t.Chdir(dir)
		got, err := ArtifactDir()
		if err != nil {
			t.Fatalf("ArtifactDir = %v", err)
		}
		want, _ := filepath.EvalSymlinks(dir)
		gotReal, _ := filepath.EvalSymlinks(got)
		if gotReal != want {
			t.Errorf("ArtifactDir = %q, want %q", gotReal, want)
		}
		if _, err := os.Stat(got); err != nil {
			t.Errorf("the directory was not there: %v", err)
		}
	})
}
