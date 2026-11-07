// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sdkTree builds an SDK-shaped directory: platforms/<names> and
// build-tools/<names>, with an android.jar in every platform unless jar is
// false.
func sdkTree(t *testing.T, platforms, buildTools []string, jar bool) string {
	t.Helper()
	home := t.TempDir()
	for _, p := range platforms {
		dir := filepath.Join(home, "platforms", p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if jar {
			if err := os.WriteFile(filepath.Join(dir, "android.jar"), []byte("jar"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, b := range buildTools {
		if err := os.MkdirAll(filepath.Join(home, "build-tools", b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// TestThePlatformChosenIsTheNewestNumberedOne.
//
// ⛔ `ls "$ANDROID_HOME/platforms" | sed -n 's/^android-\([0-9]\{1,\}\)$/\1/p' |
// sort -n | tail -1` was doing this, and every way it can go wrong produces an
// EMPTY string rather than a message: no SDK, no platform, or platforms whose
// names it did not expect. The failure then surfaced four tools later, as javac
// complaining about a class path ending in "platforms/android-/android.jar".
func TestThePlatformChosenIsTheNewestNumberedOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		dirs  []string
		want  int
		wantN string // a fragment the refusal must carry
	}{
		{name: "one platform", dirs: []string{"android-35"}, want: 35},
		{
			name: "newest wins and it is not the lexical one",
			dirs: []string{"android-9", "android-35", "android-26"},
			want: 35,
		},
		{
			// A preview platform is named for its codename and carries no API
			// level. Guessing one would be worse than skipping it.
			name: "a codename platform is skipped, not guessed at",
			dirs: []string{"android-34", "android-TiramisuPrivacySandbox"},
			want: 34,
		},
		{
			name:  "a directory of nothing but codenames is a refusal that names them",
			dirs:  []string{"android-Baklava", "temp"},
			wantN: "android-Baklava",
		},
		{
			// No platforms directory at all, which is the "no SDK here"
			// refusal rather than the "no platform in this SDK" one.
			name: "no platform at all", dirs: nil, wantN: "no Android platform",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			home := sdkTree(t, c.dirs, []string{"35.0.0"}, true)
			got, err := newestPlatform(filepath.Join(home, "platforms"))
			if c.wantN != "" {
				if err == nil {
					t.Fatalf("picked android-%d, want a refusal", got)
				}
				if !strings.Contains(err.Error(), c.wantN) {
					t.Errorf("refusal %q does not carry %q", err, c.wantN)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("chose android-%d, want android-%d", got, c.want)
			}
		})
	}
}

// TestAMissingPlatformsDirectoryIsADifferentRefusalFromAnEmptyOne, because the
// remedies differ: one installs an SDK, the other installs a platform into the
// SDK that is already there.
func TestAMissingPlatformsDirectoryIsADifferentRefusalFromAnEmptyOne(t *testing.T) {
	t.Parallel()
	_, err := newestPlatform(filepath.Join(t.TempDir(), "platforms"))
	if err == nil {
		t.Fatal("a missing platforms directory was accepted")
	}
	if !strings.Contains(err.Error(), "no Android platform") || !strings.Contains(err.Error(), "sdkmanager") {
		t.Errorf("refusal %q says neither what is missing nor how to install it", err)
	}
	home := sdkTree(t, []string{"temp"}, nil, false)
	_, err = newestPlatform(filepath.Join(home, "platforms"))
	if err == nil || !strings.Contains(err.Error(), "The directory exists") {
		t.Errorf("an empty platforms directory gave %v, want a refusal distinguishing it "+
			"from a missing SDK", err)
	}
}

// TestBuildToolsAreOrderedAsNumbersNotAsText.
//
// ⚠ `sort -V` did this correctly; a plain sort would not, and the point of the
// table is that the Go replacement keeps the property rather than that the
// shell lacked it. 9.0.0 after 35.0.0 is the failure to avoid.
func TestBuildToolsAreOrderedAsNumbersNotAsText(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		dirs []string
		want string
	}{
		{"one revision", []string{"35.0.0"}, "35.0.0"},
		{"two digits beat one", []string{"9.0.0", "35.0.0"}, "35.0.0"},
		{"the patch is compared too", []string{"35.0.0", "35.0.12", "35.0.2"}, "35.0.12"},
		{"a missing component is zero", []string{"35.0", "35.0.0"}, "35.0.0"},
		{"a release candidate loses to its release", []string{"36.0.0-rc1", "35.0.0"}, "36.0.0-rc1"},
		{"and loses the tie", []string{"35.0.0-rc1", "35.0.0"}, "35.0.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			home := sdkTree(t, []string{"android-35"}, c.dirs, true)
			got, err := newestBuildTools(filepath.Join(home, "build-tools"))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("chose %s, want %s (from %v)", got, c.want, c.dirs)
			}
		})
	}
}

func TestBuildToolsRefusals(t *testing.T) {
	t.Parallel()
	if _, err := newestBuildTools(filepath.Join(t.TempDir(), "build-tools")); err == nil {
		t.Error("a missing build-tools directory was accepted")
	}
	home := sdkTree(t, []string{"android-35"}, nil, true)
	if err := os.MkdirAll(filepath.Join(home, "build-tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := newestBuildTools(filepath.Join(home, "build-tools"))
	if err == nil || !strings.Contains(err.Error(), "holds no revision") {
		t.Errorf("an empty build-tools directory gave %v, want a refusal saying it is "+
			"empty rather than absent", err)
	}
}

// TestTheJarIsWhatIsCheckedNotTheDirectoryName: an interrupted download leaves
// the directory behind, and the name is not the artefact.
func TestTheJarIsWhatIsCheckedNotTheDirectoryName(t *testing.T) {
	t.Parallel()
	home := sdkTree(t, []string{"android-35"}, []string{"35.0.0"}, false)
	_, err := FindSDK(home)
	if err == nil {
		t.Fatal("a platform directory with no android.jar was accepted")
	}
	if !strings.Contains(err.Error(), "android.jar") {
		t.Errorf("refusal %q does not name the missing jar", err)
	}

	home = sdkTree(t, []string{"android-35"}, []string{"35.0.0"}, true)
	sdk, err := FindSDK(home)
	if err != nil {
		t.Fatal(err)
	}
	if sdk.API != 35 || sdk.BuildTools != "35.0.0" {
		t.Errorf("found %+v, want android-35 and 35.0.0", sdk)
	}
	if want := filepath.Join(home, "build-tools", "35.0.0", "aapt2"); sdk.Tool("aapt2") != want {
		t.Errorf("Tool(aapt2) = %s, want %s", sdk.Tool("aapt2"), want)
	}
	if _, err := FindSDK(""); err == nil {
		t.Error("an empty ANDROID_HOME was accepted")
	}
}

// TestPlatformLevelRefusesWhatItCannotParse covers the parser on its own, so a
// future name is a skipped directory rather than a zero that looks like an
// answer.
func TestPlatformLevelRefusesWhatItCannotParse(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"android-35", 35, true},
		{"android-7", 7, true},
		{"android-0", 0, false},
		{"android--1", 0, false},
		{"android-35x", 0, false},
		{"android-", 0, false},
		{"platforms", 0, false},
		{"35", 0, false},
	} {
		got, ok := platformLevel(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("platformLevel(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
