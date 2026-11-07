// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSDKRun stands in for the four SDK tools and the Go build. It writes what
// each one would write and records the command lines, so the ORDER and the
// ARGUMENTS are under test on a machine with no Android SDK at all.
type fakeSDKRun struct {
	t     *testing.T
	calls []string
	// env records the environment each tool was given, so that the one tool
	// that needs one can be checked rather than trusted.
	env map[string][]string
	// fail is a tool name that should return an error instead of working.
	fail string
	// skip is a tool name that should report success and write nothing, which
	// is the failure mode a build log cannot show.
	skip string
}

func (f *fakeSDKRun) Run(_ context.Context, _ string, env []string, name string, args ...string) ([]byte, error) {
	tool := filepath.Base(name)
	f.calls = append(f.calls, tool)
	if f.env == nil {
		f.env = map[string][]string{}
	}
	f.env[tool] = env
	if tool == f.fail {
		return []byte("pretend " + tool + " failed"), os.ErrInvalid
	}
	if tool == f.skip {
		return nil, nil
	}
	switch tool {
	case "go":
		// ⛔ THE FAKE BUILD OBEYS GOOS, because the real one does. Without
		// GOOS=android in the environment it writes a Mach-O, which is what
		// actually happened on 2026-11-07 when the environment was assembled
		// and never passed: the payload came out cffaedfe and mustBeELF caught
		// it. A fake that always writes ELF would have hidden the defect that
		// this signature exists to prevent.
		magic := []byte{0xcf, 0xfa, 0xed, 0xfe}
		for _, e := range env {
			if e == "GOOS=android" {
				magic = []byte{0x7f, 'E', 'L', 'F'}
			}
		}
		return nil, os.WriteFile(flagValue(args, "-o"), bytes.Repeat(magic, 4096), 0o755)
	case "javac":
		dir := flagValue(args, "-d")
		if err := os.MkdirAll(filepath.Join(dir, "org", "goxrkit"), 0o755); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(filepath.Join(dir, "org", "goxrkit", "X.class"),
			[]byte{0xca, 0xfe, 0xba, 0xbe}, 0o644)
	case "d8":
		return nil, os.WriteFile(filepath.Join(flagValue(args, "--output"), "classes.dex"),
			bytes.Repeat([]byte("dex\n"), 64), 0o644)
	case "aapt2":
		out := flagValue(args, "-o")
		if args[0] == "compile" {
			return nil, os.WriteFile(out, []byte("compiled resources"), 0o644)
		}
		fakeBase(f.t, out)
		return nil, nil
	case "keytool":
		return nil, os.WriteFile(flagValue(args, "-keystore"), []byte("keystore"), 0o600)
	case "apksigner":
		// Signing adds a block before the central directory and leaves every
		// local header where it was, which is why zipalign runs before it.
		return []byte("Signed"), nil
	case "zipalign":
		return []byte("Verifying alignment...\n   16384 lib/arm64-v8a/libxrapp.so (OK)\n" +
			"Verification succesful\n"), nil
	}
	f.t.Fatalf("the fake runner was asked for an unknown tool: %s %v", tool, args)
	return nil, nil
}

// flagValue returns the argument after flag, or "" — the fake runner reads the
// command line the same way the real tools do.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// packOptions is a build against a temporary SDK and a copy of this
// repository's host directory.
func packOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	host := filepath.Join(root, "host")
	if err := os.MkdirAll(filepath.Join(host, "java", "org", "goxrkit", "android"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "java", "org", "goxrkit", "android", "X.java"),
		[]byte("package org.goxrkit.android; class X {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "AndroidManifest.xml"), theManifest(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(host, "res", "xml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "res", "xml", "device_filter.xml"),
		[]byte("<resources/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{
		Root:        root,
		AndroidHome: sdkTree(t, []string{"android-35"}, []string{"35.0.0"}, true),
		JavaHome:    filepath.Join(root, "jdk"),
		App:         "./cmd/xrcapture",
		Edits:       Edits{Package: "org.goxrkit.test", Label: "test"},
	}
}

// TestPackRunsTheToolsInOrderAndChecksTheArtefact.
//
// The SDK's four tools are the ones only they can do; what this package owns is
// the order, the arguments, the manifest and the zip. So the order is asserted,
// and so is the APK that comes out.
func TestPackRunsTheToolsInOrderAndChecksTheArtefact(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	run := &fakeSDKRun{t: t}
	var log bytes.Buffer
	res, err := Pack(context.Background(), o, run, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	want := []string{"go", "javac", "d8", "aapt2", "aapt2", "keytool", "apksigner", "zipalign"}
	if strings.Join(run.calls, ",") != strings.Join(want, ",") {
		t.Errorf("tools ran as %v, want %v", run.calls, want)
	}
	// ⛔⛔ THE BUILD'S ENVIRONMENT IS ASSERTED, NOT ASSUMED. The first version
	// of this package built the slice and passed it nowhere; the payload came
	// out Mach-O and only the ELF check noticed. GOOS is what makes `go build`
	// cross-compile, so it is checked here as well as in the artefact.
	for _, want := range []string{"GOOS=android", "CGO_ENABLED=0", "GOARCH=arm64", "GOWORK=off"} {
		var found bool
		for _, e := range run.env["go"] {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the Go build was run without %s (environment: %v)", want, run.env["go"])
		}
	}
	// ⛔ THE APK IS THE CLAIM, NOT THE LOG. Pack's own Verify already ran; this
	// re-reads the file so the test does not take Pack's word for it.
	if err := Verify(res.APK); err != nil {
		t.Error(err)
	}
	got := entries(t, res.APK)
	lib, ok := got["lib/arm64-v8a/libxrapp.so"]
	if !ok {
		t.Fatalf("no payload in %s (entries: %d)", res.APK, len(got))
	}
	if !lib.Stored() || lib.DataOffset%PageSize != 0 {
		t.Errorf("payload stored = %v at offset %d", lib.Stored(), lib.DataOffset)
	}
	// The manifest that was linked is the SUBSTITUTED copy, and the checked-in
	// one is untouched.
	b, err := os.ReadFile(res.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `package="org.goxrkit.test"`) {
		t.Error("the linked manifest does not carry the package id asked for")
	}
	orig, err := os.ReadFile(filepath.Join(o.Root, "host", "AndroidManifest.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(orig, theManifest(t)) {
		t.Error("the checked-in manifest was modified; the substitution must go into a copy")
	}
	if res.SDK.API != 35 {
		t.Errorf("reported android-%d", res.SDK.API)
	}
	if len(res.Entries) == 0 {
		t.Error("Pack reported no entries")
	}
}

// TestPackPackagesAPreBuiltBinaryWithoutBuildingAnything, which is how an
// application from another module is packaged: the host does not care which Go
// program it spawns, and building that program is that module's business.
func TestPackPackagesAPreBuiltBinaryWithoutBuildingAnything(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	bin := filepath.Join(t.TempDir(), "xrdesk")
	if err := os.WriteFile(bin, bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 100), 0o755); err != nil {
		t.Fatal(err)
	}
	o.AppBin = bin
	o.App = ""
	run := &fakeSDKRun{t: t}
	if _, err := Pack(context.Background(), o, run, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if c == "go" {
			t.Error("a pre-built binary was given and `go build` ran anyway")
		}
	}
}

// TestAPayloadForTheWrongPlatformIsRefused.
//
// ⛔ GOOS AND GOARCH ARE STRINGS ON A COMMAND LINE. A Mach-O binary packages
// just as well as an ELF one, installs, and then is silently absent. The first
// four bytes are the only check that catches it, and no build log would.
func TestAPayloadForTheWrongPlatformIsRefused(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	bin := filepath.Join(t.TempDir(), "xrdesk")
	// Mach-O, which is what `go build` writes on this workstation with GOOS
	// unset.
	if err := os.WriteFile(bin, []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}, 0o755); err != nil {
		t.Fatal(err)
	}
	o.AppBin = bin
	o.App = ""
	_, err := Pack(context.Background(), o, &fakeSDKRun{t: t}, nil)
	if err == nil {
		t.Fatal("a Mach-O payload was packaged into an Android APK")
	}
	if !strings.Contains(err.Error(), "not an ELF") {
		t.Errorf("refusal %q does not say what is wrong with the payload", err)
	}
	// And a payload too short to have magic at all.
	if err := os.WriteFile(bin, []byte{0x7f}, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), o, &fakeSDKRun{t: t}, nil); err == nil {
		t.Error("a one-byte payload was accepted")
	}
}

// TestAToolThatSucceedsAndWritesNothingIsRefused.
//
// ⛔⛔ THIS IS THE FAILURE A BUILD LOG CANNOT SHOW. javac reporting success with
// no .class behind it, or d8 with no dex, produces an APK that installs and
// then dies at startup with hasCode="true" in its manifest. Each step checks
// its own product.
func TestAToolThatSucceedsAndWritesNothingIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ tool, want string }{
		{"javac", "wrote no .class file"},
		{"d8", "classes.dex"},
	} {
		t.Run(c.tool, func(t *testing.T) {
			t.Parallel()
			_, err := Pack(context.Background(), packOptions(t),
				&fakeSDKRun{t: t, skip: c.tool}, nil)
			if err == nil {
				t.Fatalf("%s reported success, wrote nothing, and the build went on", c.tool)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not carry %q", err, c.want)
			}
		})
	}
}

// TestEveryToolsFailureStopsTheBuild: a tool that fails must not be stepped
// over, and the refusal must name it.
func TestEveryToolsFailureStopsTheBuild(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"go", "javac", "d8", "aapt2", "apksigner", "zipalign"} {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			_, err := Pack(context.Background(), packOptions(t),
				&fakeSDKRun{t: t, fail: tool}, nil)
			if err == nil {
				t.Fatalf("%s failed and Pack returned success", tool)
			}
		})
	}
}

// TestTheKeystoreIsReusedRatherThanRegenerated.
//
// ⛔ A FRESH KEY PER BUILD CHANGES THE CERTIFICATE, and Android refuses to
// update an installed application whose signature changed
// (INSTALL_FAILED_UPDATE_INCOMPATIBLE) — so the second build of the day would
// have to be uninstalled before it could be installed.
func TestTheKeystoreIsReusedRatherThanRegenerated(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	o.Keystore = filepath.Join(t.TempDir(), "debug.keystore")
	if err := os.WriteFile(o.Keystore, []byte("an existing keystore"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := &fakeSDKRun{t: t}
	if _, err := Pack(context.Background(), o, run, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if c == "keytool" {
			t.Error("keytool ran although a keystore was already there")
		}
	}
	b, err := os.ReadFile(o.Keystore)
	if err != nil || string(b) != "an existing keystore" {
		t.Errorf("the keystore was overwritten: %q, %v", b, err)
	}
}

// TestResourcesAreOptional: a host with no res/ links without one rather than
// failing, because the resources exist for one thing — the USB device filter.
func TestResourcesAreOptional(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	if err := os.RemoveAll(filepath.Join(o.Root, "host", "res")); err != nil {
		t.Fatal(err)
	}
	run := &fakeSDKRun{t: t}
	if _, err := Pack(context.Background(), o, run, nil); err != nil {
		t.Fatal(err)
	}
	var aapt2 int
	for _, c := range run.calls {
		if c == "aapt2" {
			aapt2++
		}
	}
	if aapt2 != 1 {
		t.Errorf("aapt2 ran %d times with no resources to compile, want 1 (link only)", aapt2)
	}
}

// TestAHostWithNoJavaIsRefused: hasCode="true" with no class behind it installs
// and fails at startup.
func TestAHostWithNoJavaIsRefused(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	if err := os.RemoveAll(filepath.Join(o.Root, "host", "java")); err != nil {
		t.Fatal(err)
	}
	_, err := Pack(context.Background(), o, &fakeSDKRun{t: t}, nil)
	if err == nil {
		t.Fatal("a host with no Java source was packaged")
	}
}

// It is NOT parallel: it sets JAVA_HOME and ANDROID_HOME, which are the
// process's, and t.Setenv refuses a parallel test for exactly that reason.
func TestOptionsRefusalsAndDefaults(t *testing.T) {
	if _, err := (Options{}).withDefaults(); err == nil {
		t.Error("no repository root was accepted")
	}
	// JAVA_HOME is where javac and keytool come from; without it the build
	// would run whatever is on PATH, which on a workstation is a different JDK
	// from the one CI has.
	t.Setenv("JAVA_HOME", "")
	if _, err := (Options{Root: t.TempDir()}).withDefaults(); err == nil {
		t.Error("a missing JAVA_HOME was accepted")
	}
	t.Setenv("JAVA_HOME", "/jdk")
	t.Setenv("ANDROID_HOME", "/sdk")
	o, err := (Options{Root: "."}).withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if o.App != "./cmd/xrcapture" || o.APKName != "xrhost" || o.ABI != "arm64-v8a" ||
		o.GOARCH != "arm64" || o.Edits.Package != "org.goxrkit.androidhost" ||
		o.Edits.Label != "go-xrkit" || o.AndroidHome != "/sdk" || o.JavaHome != "/jdk" {
		t.Errorf("defaults came out as %+v", o)
	}
	if !filepath.IsAbs(o.Root) {
		t.Errorf("the root stayed relative: %s", o.Root)
	}
	if o.Out != filepath.Join(o.Root, "host", "out") {
		t.Errorf("Out defaulted to %s", o.Out)
	}
	// An application that brings its own binary needs no App default.
	o, err = (Options{Root: ".", AppBin: "/bin/x"}).withDefaults()
	if err != nil || o.App != "" {
		t.Errorf("App = %q with a pre-built binary given (err %v)", o.App, err)
	}
}

// TestPackRefusesBeforeRunningAnythingWhenTheSDKIsAbsent, so a missing SDK
// costs no forty-second Go build first.
func TestPackRefusesBeforeRunningAnythingWhenTheSDKIsAbsent(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	o.AndroidHome = t.TempDir()
	run := &fakeSDKRun{t: t}
	if _, err := Pack(context.Background(), o, run, nil); err == nil {
		t.Fatal("an empty SDK was accepted")
	}
	if len(run.calls) != 0 {
		t.Errorf("tools ran before the SDK was checked: %v", run.calls)
	}
}

// TestAnEditThatCannotBeRenderedStopsTheBuild, and does so after the payload is
// built — which is unavoidable, since the manifest is only needed at link time.
// What matters is that it stops rather than linking the unedited manifest.
func TestAnEditThatCannotBeRenderedStopsTheBuild(t *testing.T) {
	t.Parallel()
	o := packOptions(t)
	o.Edits.Permissions = []string{`android.permission.CAMERA" x="y`}
	run := &fakeSDKRun{t: t}
	_, err := Pack(context.Background(), o, run, nil)
	if err == nil {
		t.Fatal("a permission that would inject an attribute was packaged")
	}
	for _, c := range run.calls {
		if c == "apksigner" {
			t.Error("the build reached signing with a manifest it could not render")
		}
	}
}

// TestExecRunnerCarriesTheEnvironmentItIsGiven. The interface takes an
// environment because the first version dropped one; this is the real runner
// proving it does not, by asking `go env` for a variable only the call set.
func TestExecRunnerCarriesTheEnvironmentItIsGiven(t *testing.T) {
	t.Parallel()
	out, err := ExecRunner{}.Run(context.Background(), ".", nil, "go", "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "go version") {
		t.Errorf("go version printed %q", out)
	}
	out, err = ExecRunner{}.Run(context.Background(), ".",
		[]string{"GOOS=android", "GOARCH=arm64"}, "go", "env", "GOOS", "GOARCH")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); len(got) != 2 || got[0] != "android" || got[1] != "arm64" {
		t.Errorf("`go env GOOS GOARCH` under GOOS=android answered %q — the environment "+
			"did not reach the command, which is the defect this parameter exists for", out)
	}
	_, err = ExecRunner{}.Run(context.Background(), ".", []string{"X=1"}, "go", "notacommand")
	if err == nil {
		t.Fatal("a failing command was reported as success")
	}
	if !strings.Contains(err.Error(), "go") {
		t.Errorf("refusal %q does not name the program", err)
	}
}

// TestMergeEnvReplacesRatherThanAppends.
//
// ⛔⛔ THIS IS THE SECOND HALF OF THE SAME DEFECT. Two entries for one name are
// resolved by the C library, which takes the FIRST, so an appended PATH leaves
// the process's own in force — and `d8` is a wrapper script whose `java` comes
// from PATH. The measured symptom was "class file version 55.0" from a Java 8
// on this workstation's PATH, one step after javac had compiled for 17.
func TestMergeEnvReplacesRatherThanAppends(t *testing.T) {
	t.Parallel()
	base := []string{"PATH=/usr/bin", "HOME=/home/x", "GOOS=darwin"}
	got := mergeEnv(base, []string{"PATH=/jdk/bin:/usr/bin", "GOOS=android", "NEW=1"})
	want := []string{"HOME=/home/x", "PATH=/jdk/bin:/usr/bin", "GOOS=android", "NEW=1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("mergeEnv gave\n  %v\nwant\n  %v", got, want)
	}
	// Each overridden name appears ONCE: a second entry is what made the
	// override invisible.
	for _, n := range []string{"PATH=", "GOOS="} {
		var count int
		for _, kv := range got {
			if strings.HasPrefix(kv, n) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s appears %d times in the merged environment", n, count)
		}
	}
	// A nameless entry is kept rather than dropped or treated as a key.
	if got := mergeEnv([]string{"ODD"}, nil); len(got) != 1 || got[0] != "ODD" {
		t.Errorf("mergeEnv lost a malformed entry: %v", got)
	}
	if got := mergeEnv(nil, []string{"A=1"}); len(got) != 1 {
		t.Errorf("mergeEnv(nil, …) = %v", got)
	}
}

// TestTheToolPathPutsTheJDKAndTheBuildToolsFirst, because d8, apksigner and
// zipalign are wrapper scripts and an absolute path to the wrapper says
// nothing about which java it will exec.
func TestTheToolPathPutsTheJDKAndTheBuildToolsFirst(t *testing.T) {
	t.Parallel()
	o := Options{JavaHome: "/jdk"}
	sdk := SDK{Home: "/sdk", API: 35, BuildTools: "35.0.0"}
	got := toolPath(o, sdk)
	if !strings.HasPrefix(got, "PATH=") {
		t.Fatalf("toolPath returned %q", got)
	}
	parts := strings.Split(strings.TrimPrefix(got, "PATH="), string(os.PathListSeparator))
	if parts[0] != filepath.Join("/jdk", "bin") {
		t.Errorf("the JDK is not first: %v", parts)
	}
	if parts[1] != filepath.Join("/sdk", "build-tools", "35.0.0") {
		t.Errorf("the build-tools directory is not second: %v", parts)
	}
	if len(parts) < 3 {
		t.Error("the process's own PATH was dropped rather than appended to")
	}
}

func TestCopyFileRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	if err := copyFile(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("copying a missing file was accepted")
	}
}

func TestFilesUnderIsSortedAndRefusesAMissingDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"b.java", "a.java", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := filesUnder(dir, ".java")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || filepath.Base(got[0]) != "a.java" || filepath.Base(got[1]) != "b.java" {
		t.Errorf("filesUnder gave %v, want a.java then b.java", got)
	}
	if _, err := filesUnder(filepath.Join(dir, "absent"), ".java"); err == nil {
		t.Error("a missing directory was accepted")
	}
}
