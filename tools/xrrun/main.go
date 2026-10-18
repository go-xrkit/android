// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrrun runs one of this repository's probes on a device and brings its
// transcript back.
//
// # Why it exists
//
// Every measurement in this package's README was taken by hand, with this:
//
//	APP=./cmd/xrusb PERMISSIONS=… host/build.sh
//	adb -s … install -r host/out/xrhost.apk
//	adb -s … shell run-as org.goxrkit.androidhost rm -f files/usb.txt
//	adb -s … shell am force-stop org.goxrkit.androidhost
//	adb -s … shell "am start -n …/.XrDemoActivity --ez capture false --es args '…'"
//	sleep 16
//	adb -s … shell run-as org.goxrkit.androidhost cat files/usb.txt
//
// Seven steps, run more than a dozen times in two days, and MISTAKES WERE MADE
// IN IT RATHER THAN IN THE CODE UNDER TEST: a transcript read before it was
// written and reported as "the probe said nothing", a stale APK measured
// because the install was skipped, and an argument string the shell split into
// `pkg=5m`. Each cost a cycle and one of them cost a wrong conclusion.
//
// ⛔ AND IT IS GO, NOT A SHELL SCRIPT. The steps have real conditions between
// them — wait for the device, wait for the FILE to appear rather than for a
// duration, notice a crash instead of timing out on it — and a script that
// sleeps cannot tell "not finished" from "died". `adb` is still the transport;
// what moves into Go is the deciding.
//
//	go run ./tools/xrrun -app ./cmd/xrusb -args '-wait 30s' -perms android.permission.CAMERA
//
// It prints the transcript and exits non-zero when the probe did.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-appdirs/outdir"
)

// pkg is the APK this repository builds, and the one host/build.sh defaults to.
const pkg = "org.goxrkit.androidhost"

// activity is the one that spawns the Go binary; see XrDemoActivity.
const activity = pkg + "/org.goxrkit.android.XrDemoActivity"

// transcripts maps a probe to the file it writes, so -app alone is enough.
//
// ⚠ IT IS A TABLE RATHER THAN A FLAG because getting it wrong is silent: a
// wrong name reads as "the probe wrote nothing", which is what a crash looks
// like too.
var transcripts = map[string]string{
	"xrusb":     "usb.txt",
	"xrhead":    "head.txt",
	"xrcam":     "camera.txt",
	"xrscreen":  "screen.txt",
	"xrglasses": "glasses.txt",
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "xrrun:", err)
		os.Exit(1)
	}
}

type options struct {
	app      string
	args     string
	perms    string
	device   string
	patience time.Duration
	build    bool
	install  bool
	pull     bool
}

func run(ctx context.Context, argv []string) error {
	var o options
	fs := flag.NewFlagSet("xrrun", flag.ContinueOnError)
	fs.StringVar(&o.app, "app", "./cmd/xrusb", "the probe to package and run")
	fs.StringVar(&o.args, "args", "", "arguments for the probe, as one string")
	fs.StringVar(&o.perms, "perms", "", "space-separated permissions the APK should declare")
	fs.StringVar(&o.device, "device", "", "adb device, or empty for the only one")
	fs.DurationVar(&o.patience, "patience", 3*time.Minute, "how long to wait for the transcript")
	fs.BoolVar(&o.build, "build", true, "build the APK first")
	fs.BoolVar(&o.install, "install", true, "install it")
	// ⭐ THE FRAMES COME BACK, AND WHERE THEY LAND IS NOT A PREFERENCE. A frame
	// from a headset's camera is a picture of a room and whoever is in it. The
	// choice of directory belongs to go-appdirs/outdir, which already owns this
	// question for the fleet: somewhere durable, outside every work tree, with
	// the path resolved first — a capture directory reached through a symbolic
	// link found no .git in an earlier copy of this rule and was accepted.
	fs.BoolVar(&o.pull, "pull", false, "bring back any PNG frames the probe wrote")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	name := filepath.Base(o.app)
	file, ok := transcripts[name]
	if !ok {
		return fmt.Errorf("%s writes no transcript this tool knows; add it to the table "+
			"in tools/xrrun rather than guessing a name", name)
	}
	if o.device == "" {
		d, err := soleDevice(ctx)
		if err != nil {
			return err
		}
		o.device = d
		fmt.Printf("==> device %s\n", d)
	}

	// ⛔ ASKED BEFORE THE BUILD, because the build takes forty seconds and a
	// locked phone refuses at the end of them. It is asked AGAIN below, just
	// before the start, because a phone locks itself during a build.
	if err := awake(ctx, o.device); err != nil {
		return err
	}

	if o.build {
		fmt.Printf("==> building %s\n", o.app)
		if err := build(ctx, o); err != nil {
			return err
		}
	}
	if o.install {
		fmt.Println("==> installing")
		if _, err := adb(ctx, o.device, "install", "-r", "host/out/xrhost.apk"); err != nil {
			return err
		}
	}
	if o.perms != "" {
		for _, p := range strings.Fields(o.perms) {
			// A permission the manifest declares still has to be granted; a
			// dangerous one is never granted by installing.
			if _, err := adb(ctx, o.device, "shell", "pm", "grant", pkg, p); err != nil {
				return fmt.Errorf("granting %s: %w", p, err)
			}
		}
	}

	// ⛔ REMOVE THE OLD TRANSCRIPT BEFORE STARTING, AND WAIT FOR THE FILE
	// ITSELF. Reading a stale one is how a fixed probe was reported as still
	// broken, and sleeping for a guessed duration is how an unfinished one was
	// reported as silent.
	if _, err := adb(ctx, o.device, "shell", "run-as", pkg, "rm", "-f", "files/"+file); err != nil {
		return err
	}
	if _, err := adb(ctx, o.device, "shell", "am", "force-stop", pkg); err != nil {
		return err
	}
	if _, err := adb(ctx, o.device, "logcat", "-c", "-b", "all"); err != nil {
		return err
	}

	start := []string{"shell", "am", "start", "-n", activity, "--ez", "capture", "false"}
	if o.args != "" {
		// ⛔ ONE ARGUMENT, QUOTED FOR THE DEVICE'S SHELL. `adb shell` re-splits
		// what it is given, and an unquoted "-wait 5m" arrived as a package
		// name: `am` read `5m` as its target and started nothing. exec.Command
		// keeps it one argv entry here; the quoting is for the far side.
		start = append(start, "--es", "args", "'"+o.args+"'")
	}
	fmt.Printf("==> starting %s %s\n", name, o.args)
	if err := awake(ctx, o.device); err != nil {
		return err
	}
	if _, err := adb(ctx, o.device, start...); err != nil {
		return err
	}

	text, err := waitForTranscript(ctx, o, file)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Print(text)
	// ⛔ THE FRAMES COME BACK BEFORE THE VERDICT IS ACTED ON. A run that ends in
	// FAIL is exactly the one whose pictures somebody wants to look at, and
	// returning first would leave them on the phone.
	if o.pull {
		if err := pullFrames(ctx, o); err != nil {
			fmt.Fprintln(os.Stderr, "xrrun:", err)
		}
	}
	if strings.Contains(text, "FAIL") || strings.Contains(text, "STOPPED") {
		return errors.New("the probe did not finish its measurement; the transcript is above")
	}
	return nil
}

// safeFrameName reports whether a name the DEVICE supplied may be used as a
// filename here.
//
// ⛔⛔ THE NAMES COME FROM THE PHONE, AND THE FIRST VERSION OF THIS TOOL JOINED
// THEM TO A PATH AFTER CHECKING ONLY A PREFIX AND A SUFFIX. That is a path
// traversal, and it was measured rather than reasoned about:
//
//	"frame-/../../../tmp/x.png"                 -> …/Application Support/tmp/x.png
//	"frame-/../../../../../../etc/cron.d/x.png" -> /Users/etc/cron.d/x.png
//
// Both pass "begins with frame-, ends with .png", because `frame-` is a path
// element and the `..` that follow are their own. A real filesystem cannot
// produce a basename containing a separator — but these arrive over `adb shell
// ls` from a device, across a WIRELESS link, and the write happens with the
// person's own privileges on their own workstation. A tool that pulls files off
// a device must treat their names as input.
//
// ⚠ IT IS TWO CHECKS AND NOT ONE, deliberately: the name must be a single path
// element, AND the joined path must still be inside the directory. The second
// does not trust the first, because the first is a rule about strings and the
// second is a question about the filesystem — and it is the second that would
// catch a case neither of us thought of.
func safeFrameName(name string) bool {
	if !strings.HasPrefix(name, "frame-") || !strings.HasSuffix(name, ".png") {
		return false
	}
	// One path element: no separator, and nothing a cleaner would rewrite.
	return name == filepath.Base(name) && !strings.ContainsRune(name, '/') &&
		!strings.ContainsRune(name, filepath.Separator) && name != ".." && name != "."
}

// insideDir reports whether path is within dir, which is the check that does not
// trust the name at all.
// ⚠ STRICTLY WITHIN: the directory itself is not a place a FILE can go, so "."
// is refused along with ".." and anything above. And an error from Rel — which
// is what comparing a relative path with an absolute one gives — reads as "not
// inside" rather than as "inside", because a comparison that could not be made
// is not a yes.
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pullFrames brings the probe's PNG frames back, into a directory that is
// durable and is not in any work tree.
//
// ⛔⛔ WHERE THEY LAND IS THE WHOLE POINT, AND IT IS NOT THIS FILE'S DECISION.
// A frame from a headset's camera is a picture of a room and whoever is in it,
// and this fleet has already written a capture of a whole desktop into a public
// repository's testdata/. go-appdirs/outdir owns the question — durable, outside
// every work tree, path RESOLVED first, because a directory reached through a
// symbolic link found no .git in an earlier hand-rolled copy of the rule and was
// accepted. /tmp on macOS is such a link.
func pullFrames(ctx context.Context, o options) error {
	dir, err := outdir.Ensure(outdir.Spec{
		App: "go-xrkit-android",
		Env: "XRKIT_FRAME_DIR",
		Sub: "frames",
	})
	if err != nil {
		return fmt.Errorf("choosing somewhere to put the frames: %w", err)
	}
	// run-as is how the app's private directory is readable at all; see
	// android.SaveTranscript for why the frames are in there and not in the
	// external one. `adb pull` cannot reach it, so each file is read out.
	out, err := adb(ctx, o.device, "shell", "run-as", pkg, "ls", "files")
	if err != nil {
		return err
	}
	names := strings.Fields(string(out))
	pulled := 0
	for _, name := range names {
		if !safeFrameName(name) {
			continue
		}
		raw, err := adb(ctx, o.device, "exec-out", "run-as", pkg, "cat", "files/"+name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		// ⛔ AND THE JOINED PATH IS CHECKED, not just the name. See safeFrameName:
		// the two checks are independent on purpose, and this is the one that
		// asks the filesystem rather than the string.
		out := filepath.Join(dir, name)
		if !insideDir(dir, out) {
			return fmt.Errorf("the device offered a frame called %q, which lands at %q "+
				"— outside the directory chosen for it. Nothing was written",
				name, out)
		}
		if err := os.WriteFile(out, raw, 0o600); err != nil {
			return err
		}
		pulled++
	}
	if pulled == 0 {
		fmt.Printf("==> no frames to bring back (the probe writes them with -keep)\n")
		return nil
	}
	fmt.Printf("==> %d frame(s) in %s\n", pulled, dir)
	fmt.Printf("    they are pictures of a room: they are outside every work tree, and " +
		"deleting them when you are done is the whole of the hygiene\n")
	return nil
}

// waitForTranscript waits for the probe to write its verdict, and says WHY it
// gave up rather than only that it did.
func waitForTranscript(ctx context.Context, o options, file string) (string, error) {
	fmt.Printf("==> waiting up to %s for files/%s\n", o.patience, file)
	deadline := time.Now().Add(o.patience)
	for {
		out, err := adb(ctx, o.device, "shell", "run-as", pkg, "cat", "files/"+file)
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return string(out), nil
		}
		// ⭐ A CRASH IS NOT A TIMEOUT, and waiting out the patience for one
		// wastes minutes and reports the wrong thing. The crash buffer names
		// the exception and the line; a BackgroundServiceStartNotAllowed was
		// read as "the probe said nothing" for a whole cycle.
		if crash, cerr := adb(ctx, o.device, "logcat", "-d", "-b", "crash"); cerr == nil {
			if i := bytes.Index(crash, []byte(pkg)); i >= 0 {
				return "", fmt.Errorf("the application crashed instead of measuring:\n%s",
					firstLines(crash[i:], 6))
			}
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("files/%s never appeared in %s, and the application did "+
				"not crash; it is probably still waiting for something", file, o.patience)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// awake refuses to run on a phone that is asleep or locked, and says which.
//
// ⛔⛔ THIS COST THREE MEASUREMENT CYCLES, EVERY ONE OF THEM AT THE END. Behind
// the keyguard the platform does not credit the application as foreground,
// however resumed its Activity is, so startService throws and the process dies
// in onResume:
//
//	BackgroundServiceStartNotAllowedException: app is in background
//	  uid u0a350 ... bg:+107ms idle
//	mWakefulness=Dozing
//	isKeyguardShowing=true
//
// Read from the far side first, that is one line before anything is built. And
// it reads BOTH conditions because they are different phones' problems: a dark
// screen (Dozing, Asleep) and an unlocked-but-covered one (keyguard showing)
// each refuse a service start on their own, and a check that read only the
// wakefulness passed on a phone that then refused.
//
// ⚠ A DEVICE THAT CANNOT BE ASKED IS ALLOWED THROUGH. dumpsys sections get
// renamed between releases, and a tool that refused to run because it could not
// find a field would be worse than the crash it is avoiding — the crash at
// least names its own cause.
func awake(ctx context.Context, device string) error {
	// ⛔⛔ BOTH SECTIONS, BECAUSE THE TWO FACTS LIVE IN DIFFERENT ONES. The
	// wakefulness is in `dumpsys power` and the keyguard is in `dumpsys window
	// policy`, which I learned by READING THE PHONE rather than from the crash
	// transcript: the first version of this grepped only `window policy` for
	// `mWakefulness=Dozing`, a string that section never prints, and would have
	// passed a phone that was asleep at that moment.
	var text string
	for _, section := range [][]string{
		{"shell", "dumpsys", "power"},
		{"shell", "dumpsys", "window", "policy"},
	} {
		out, err := adb(ctx, device, section...)
		if err != nil {
			continue // see the note above: unreadable is not a refusal
		}
		text += string(out)
	}
	why := lockedBecause(text)
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("%s is not usable: %s.\n\n"+
		"⛔ WAKE AND UNLOCK THE PHONE, AND LEAVE IT ON. Android does not credit a locked "+
		"application as foreground, so the host's own service start is refused and the "+
		"probe dies before it measures anything. There is no flag for this and there "+
		"should not be: the platform is right and the phone is the thing to change.\n"+
		"  adb -s %s shell svc power stayon usb   # keeps it awake while plugged in",
		device, strings.Join(why, " and "), device)
}

// lockedBecause names every reason the dumpsys output gives for a phone being
// unusable, and is separate from [awake] so it can be tested without a phone.
//
// ⛔ BOTH CONDITIONS ARE READ, NOT ONE. An asleep phone and an awake-but-locked
// one each refuse a service start on their own, and reading only the wakefulness
// passes on the second. They are returned together, because a person told "it is
// asleep" who then wakes it and is told "it is locked" has been sent round twice
// for one problem.
//
// ⛔⛔ AND EVERY STRING HERE WAS READ OFF THE PHONE, not taken from the crash
// transcript. Measured on a Pixel 11 Pro Fold on 2026-10-11, asleep and locked:
//
//	dumpsys power          mWakefulness=Dozing        mIsInteractive: false
//	dumpsys window policy  showing=true               inputRestricted=true
//	                       interactiveState=INTERACTIVE_STATE_SLEEP
//	                       screenState=SCREEN_STATE_ON   ← always-on display
//
// Three things that transcript corrects. `mWakefulness` is in `dumpsys power`
// and NOT in `window policy`, which the first version of this grepped alone.
// `isKeyguardShowing` appears in neither — the keyguard says `showing=true`
// inside its own delegate block. And `screenState` is ON while the phone sleeps,
// so a check on the screen would have called this phone usable.
func lockedBecause(dumpsys string) []string {
	var why []string
	for _, c := range []struct {
		say   string
		signs []string
	}{
		{"it is asleep", []string{
			"mWakefulness=Dozing", "mWakefulness=Asleep",
			"mIsInteractive: false", "INTERACTIVE_STATE_SLEEP",
		}},
		// inputRestricted is the keyguard's own word for what it is doing, and
		// it is what makes the application not foreground. isKeyguardShowing is
		// kept for the releases that print it.
		{"it is locked", []string{
			"inputRestricted=true", "isKeyguardShowing=true", "mShowingLockscreen=true",
		}},
	} {
		for _, s := range c.signs {
			if strings.Contains(dumpsys, s) {
				why = append(why, c.say)
				break
			}
		}
	}
	return why
}

// soleDevice returns the only attached device, refusing to choose.
//
// ⛔ IT REFUSES RATHER THAN PICKING THE FIRST. Wireless adb leaves a device
// listed twice — once by address and once by mDNS name — and a tool that
// guessed would install on one and read from the other.
func soleDevice(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "adb", "devices").Output()
	if err != nil {
		return "", fmt.Errorf("adb devices: %w", err)
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "device" {
			found = append(found, f[0])
		}
	}
	switch len(found) {
	case 0:
		return "", errors.New("no device is attached; plug one in or `adb connect` it")
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%d devices are attached (%s); name one with -device. Wireless adb "+
		"lists the same phone twice, and `adb disconnect <ip>:<port>` leaves the mDNS "+
		"entry, which survives an address change", len(found), strings.Join(found, ", "))
}

// build runs host/build.sh, which is the SDK's own tool chain and stays a
// script: it is aapt2, javac, d8, zipalign and apksigner in a row, with no
// decisions in it.
func build(ctx context.Context, o options) error {
	env := append(os.Environ(), "APP="+o.app, "GOWORK=off")
	if o.perms != "" {
		env = append(env, "PERMISSIONS="+o.perms)
	}
	// ⛔ FIND THE TOOL CHAIN RATHER THAN DEMANDING IT. build.sh refuses without
	// ANDROID_HOME and JAVA_HOME, and a tool that needs two exports before it
	// will run is a tool nobody runs — the seven-step incantation this replaces
	// was itself only ever typed with them already in the shell.
	if os.Getenv("ANDROID_HOME") == "" {
		p, err := findDir("ANDROID_HOME", "platforms", androidHomes())
		if err != nil {
			return err
		}
		fmt.Printf("==> ANDROID_HOME %s\n", p)
		env = append(env, "ANDROID_HOME="+p)
	}
	if os.Getenv("JAVA_HOME") == "" {
		p, err := findDir("JAVA_HOME", filepath.Join("bin", "javac"), javaHomes())
		if err != nil {
			return err
		}
		fmt.Printf("==> JAVA_HOME %s\n", p)
		env = append(env, "JAVA_HOME="+p)
	}
	cmd := exec.CommandContext(ctx, "sh", "host/build.sh")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("host/build.sh: %w\n%s", err, out)
	}
	return nil
}

func adb(ctx context.Context, device string, args ...string) ([]byte, error) {
	full := append([]string{"-s", device}, args...)
	out, err := exec.CommandContext(ctx, "adb", full...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("adb %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

func firstLines(b []byte, n int) string {
	lines := bytes.SplitN(b, []byte("\n"), n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return string(bytes.Join(lines, []byte("\n")))
}

// findDir returns the first candidate holding want, or says what was looked for
// and where — an error that only says "not found" leaves the reader to guess
// which of two variables and which of five directories.
func findDir(name, want string, candidates []string) (string, error) {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, want)); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s is not set and no directory holding %q was found; looked in "+
		"%s. Set %s, or install the tool chain — pkgx has both: "+
		"`pkgx +android.com/cmdline-tools +openjdk.org`",
		name, want, strings.Join(candidates, ", "), name)
}

// androidHomes are the places an Android SDK is found on this fleet's machines.
// The shared one is first because a CI image and a workstation rarely carry the
// same revisions, and a shared SDK is the one both agree on.
func androidHomes() []string {
	home, _ := os.UserHomeDir()
	return []string{
		"/Users/Shared/android-sdk",
		filepath.Join(home, "Library", "Android", "sdk"),
		filepath.Join(home, "Android", "Sdk"),
		"/opt/homebrew/share/android-commandlinetools",
		"/usr/local/share/android-commandlinetools",
	}
}

// javaHomes are the places a JDK is found. pkgx keeps versioned directories, so
// the newest is taken rather than a name guessed.
func javaHomes() []string {
	home, _ := os.UserHomeDir()
	out := []string{os.Getenv("JDK_HOME")}
	if ms, err := filepath.Glob(filepath.Join(home, ".pkgx", "openjdk.org", "v*")); err == nil {
		// Glob sorts lexically, which puts v9 after v21; the last entry is
		// taken anyway because any JDK here compiles this host's three files.
		out = append(out, ms...)
	}
	return append(out,
		"/Library/Java/JavaVirtualMachines/temurin-21.jdk/Contents/Home",
		"/opt/homebrew/opt/openjdk",
		"/usr/lib/jvm/default-java",
	)
}
