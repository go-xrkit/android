// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Package xrpack packages the host APK: the Java host compiled by the SDK's own
// tool chain, a CGO-free Go binary as the payload, and a zip written here.
//
// It replaces host/build.sh, which was 130 lines of shell and had decisions in
// it — which platform, which build-tools, whether to build or to copy, whether a
// keystore exists — plus three kinds of text processing: `ls | sed | sort -n |
// tail -1` to pick an API level, `sed` to substitute the package id and the
// label into the manifest, and `zip` to add the payload. A pipeline cannot tell
// "no platform installed" from "a platform whose name I did not expect": both
// leave the variable empty, and the failure surfaces four tools later as a
// javac error about a missing android.jar.
//
// The zip is written here rather than by `zip` + `zipalign` because the
// alignment that matters cannot be reached from outside: zipalign aligns
// UNCOMPRESSED .so entries, and `zip` deflates by default, so the check it
// offers passes without looking. Measured on this repository's own APK,
// 2026-11-07, build-tools 35.0.0:
//
//	zipalign -c -P 16 -v 4 host/out/xrhost.apk
//	   35831 lib/arm64-v8a/libxrapp.so (OK - compressed)
//	Verification succesful
//
// "OK - compressed" is not a verdict about alignment. The payload is stored
// uncompressed and page-aligned here, which makes that same command a real
// check — and Verify refuses an entry reported as compressed for exactly that
// reason.
package xrpack

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SDK is the part of an Android SDK this packager uses: one platform's
// android.jar and one build-tools revision's executables.
type SDK struct {
	Home       string // ANDROID_HOME
	API        int    // the platform's API level, e.g. 35
	BuildTools string // the build-tools revision, e.g. "35.0.0"
}

// AndroidJar is the compile-time class path, and the library aapt2 and d8 link
// against.
func (s SDK) AndroidJar() string {
	return filepath.Join(s.Home, "platforms", "android-"+strconv.Itoa(s.API), "android.jar")
}

// Tool returns the path of a build-tools executable by name.
func (s SDK) Tool(name string) string {
	return filepath.Join(s.Home, "build-tools", s.BuildTools, name)
}

// FindSDK picks the newest platform and the newest build-tools revision under
// home.
//
// ⛔ NEWEST INSTALLED IS THE ANSWER, NOT A PINNED REVISION: a CI image and a
// workstation rarely carry the same ones, and pinning means one of the two
// cannot build at all. What must not happen is picking SILENTLY when there is
// nothing to pick, which is what the shell did.
func FindSDK(home string) (SDK, error) {
	if home == "" {
		return SDK{}, fmt.Errorf("no Android SDK: ANDROID_HOME is empty")
	}
	api, err := newestPlatform(filepath.Join(home, "platforms"))
	if err != nil {
		return SDK{}, err
	}
	bt, err := newestBuildTools(filepath.Join(home, "build-tools"))
	if err != nil {
		return SDK{}, err
	}
	s := SDK{Home: home, API: api, BuildTools: bt}
	// ⛔ AND THE JAR IS THE THING ACTUALLY NEEDED. A platform directory can
	// exist with its download interrupted; the name is not the artefact.
	if _, err := os.Stat(s.AndroidJar()); err != nil {
		return SDK{}, fmt.Errorf("platform android-%d is installed but %s is missing: %w",
			api, s.AndroidJar(), err)
	}
	return s, nil
}

// newestPlatform returns the highest API level among the android-<n>
// directories, naming what it saw when there is none — an empty answer and a
// directory full of unexpected names are different problems with different
// remedies.
func newestPlatform(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("no Android platform: reading %s: %w\n"+
			"install one — `pkgx +android.com/cmdline-tools` then "+
			"`sdkmanager \"platforms;android-35\"`", dir, err)
	}
	best, saw := 0, []string(nil)
	for _, e := range ents {
		name := e.Name()
		saw = append(saw, name)
		n, ok := platformLevel(name)
		if !ok {
			continue
		}
		if n > best {
			best = n
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("no Android platform under %s: %d entr(ies), none named "+
			"android-<n> (%s). The directory exists, so this is not a missing SDK but a "+
			"missing platform: `sdkmanager \"platforms;android-35\"`",
			dir, len(saw), strings.Join(saw, ", "))
	}
	return best, nil
}

// platformLevel parses an "android-<n>" directory name. Preview platforms are
// named for their codename ("android-TiramisuPrivacySandbox") and carry no
// number; they are skipped rather than guessed at, because a preview's API
// level is not derivable from its name.
func platformLevel(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "android-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// newestBuildTools returns the highest build-tools revision.
//
// ⚠ `sort -V` was doing this, and a lexical sort would put "9.0.0" after
// "35.0.0". The components are compared as NUMBERS, and a revision carrying a
// suffix ("35.0.0-rc1") compares by its numeric part and loses the tie to the
// release, which is the conservative answer: a release candidate should not win
// over the release it precedes.
func newestBuildTools(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no Android build-tools: reading %s: %w\n"+
			"install one — `sdkmanager \"build-tools;35.0.0\"`", dir, err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no build-tools revision under %s: the directory exists and "+
			"holds no revision, so `sdkmanager \"build-tools;35.0.0\"` is what is missing", dir)
	}
	sort.Slice(names, func(i, j int) bool { return lessRevision(names[i], names[j]) })
	return names[len(names)-1], nil
}

// lessRevision orders two dotted revisions numerically.
func lessRevision(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		an, apre := component(as, i)
		bn, bpre := component(bs, i)
		if an != bn {
			return an < bn
		}
		if apre != bpre {
			// A pre-release component sorts BELOW the plain one it qualifies.
			return apre
		}
	}
	return false
}

// component parses the i-th dotted component, reporting whether it carried a
// pre-release suffix. A missing component is zero, so "35.0" and "35.0.0" are
// the same revision.
func component(parts []string, i int) (n int, pre bool) {
	if i >= len(parts) {
		return 0, false
	}
	p := parts[i]
	digits := 0
	for digits < len(p) && p[digits] >= '0' && p[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, true
	}
	n, _ = strconv.Atoi(p[:digits])
	return n, digits != len(p)
}
