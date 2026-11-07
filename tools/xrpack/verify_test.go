// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// plainZip writes an APK the way `zip` does: every entry through the default
// path, which deflates. It is the CONTROL for every claim below — the state
// this package replaced.
func plainZip(t *testing.T, path string, method uint16, names ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := zip.NewWriter(f)
	for _, n := range names {
		hw, err := w.CreateHeader(&zip.FileHeader{Name: n, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hw.Write(bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 2000)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRefusesACompressedLibrary(t *testing.T) {
	t.Parallel()
	apk := filepath.Join(t.TempDir(), "deflated.apk")
	plainZip(t, apk, zip.Deflate, "lib/arm64-v8a/libxrapp.so")
	err := Verify(apk)
	if err == nil {
		t.Fatal("an APK with a compressed library was accepted, which is exactly what " +
			"zipalign does when it prints \"OK - compressed\"")
	}
	if !strings.Contains(err.Error(), "COMPRESSED") {
		t.Errorf("refusal %q does not say the library is compressed", err)
	}
}

func TestVerifyRefusesAStoredButMisalignedLibrary(t *testing.T) {
	t.Parallel()
	apk := filepath.Join(t.TempDir(), "unaligned.apk")
	// Stored, and written by archive/zip with no padding, so its offset is
	// 30 + len(name) + 9 bytes of extended timestamp — not a multiple of 16384
	// for any name.
	plainZip(t, apk, zip.Store, "lib/arm64-v8a/libxrapp.so")
	err := Verify(apk)
	if err == nil {
		t.Fatal("a stored but misaligned library was accepted")
	}
	if !strings.Contains(err.Error(), "16 KiB pages") {
		t.Errorf("refusal %q does not say what the alignment is for", err)
	}
}

// TestVerifyRefusesAnAPKWithNoLibraryAtAll.
//
// ⛔⛔ EVERY OTHER REFUSAL IN Verify IS ABOUT AN ENTRY THAT IS PRESENT, so an
// APK holding none satisfies them all. That is how a conformance claim becomes
// vacuous: the subject leaves and the gate reports success.
func TestVerifyRefusesAnAPKWithNoLibraryAtAll(t *testing.T) {
	t.Parallel()
	apk := filepath.Join(t.TempDir(), "empty.apk")
	plainZip(t, apk, zip.Store, "classes.dex")
	err := Verify(apk)
	if err == nil {
		t.Fatal("an APK with no payload passed")
	}
	if !strings.Contains(err.Error(), "empty subject") {
		t.Errorf("refusal %q does not say why an absent payload is not a pass", err)
	}
	if err := Verify(filepath.Join(t.TempDir(), "absent.apk")); err == nil {
		t.Error("a missing APK passed")
	}
}

func TestInspectReportsWhereTheDataStarts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(t, base)
	out := filepath.Join(dir, "out.apk")
	if err := Repack(base, out, []Add{
		{Name: "lib/arm64-v8a/libxrapp.so", Data: []byte("elf"), Store: true},
	}); err != nil {
		t.Fatal(err)
	}
	es, err := Inspect(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 4 {
		t.Fatalf("Inspect reported %d entries, want 4", len(es))
	}
	for _, e := range es {
		if e.DataOffset <= 0 {
			t.Errorf("%s reports its data at %d", e.Name, e.DataOffset)
		}
	}
	if _, err := Inspect(filepath.Join(dir, "absent.apk")); err == nil {
		t.Error("Inspect accepted a missing file")
	}
}

// TestTheSDKAgreesAndCanTellTheTwoApart is the independent witness: the SDK's
// own zipalign, not this package's arithmetic.
//
// ⛔⛔ AND IT CARRIES ITS OWN CONTROL. A check that only ever sees a good APK
// cannot show that it would notice a bad one — so the compressed-library APK is
// run through the same command, and the test asserts that zipalign EXITS ZERO
// on it while saying "compressed". That exit status is the reason
// zipalignAgrees reads the output instead of the status.
func TestTheSDKAgreesAndCanTellTheTwoApart(t *testing.T) {
	t.Parallel()
	zipalign := findZipalign(t)
	dir := t.TempDir()

	base := filepath.Join(dir, "base.apk")
	fakeBase(t, base)
	good := filepath.Join(dir, "good.apk")
	if err := Repack(base, good, []Add{
		{Name: "lib/arm64-v8a/libxrapp.so",
			Data: bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 9000), Store: true},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(zipalign, "-c", "-P", "16", "-v", "4", good).CombinedOutput()
	if err != nil {
		t.Fatalf("zipalign refused the APK this package wrote: %v\n%s", err, out)
	}
	line := libLine(string(out))
	if line == "" {
		t.Fatalf("zipalign listed no library:\n%s", out)
	}
	if strings.Contains(line, "compressed") {
		t.Errorf("zipalign says %q — the payload is compressed after all", line)
	}
	if !strings.Contains(line, "OK") {
		t.Errorf("zipalign says %q", line)
	}

	// The control: the state before this package. Same command, same exit
	// status, a different word in the middle of the line.
	bad := filepath.Join(dir, "deflated.apk")
	plainZip(t, bad, zip.Deflate, "lib/arm64-v8a/libxrapp.so")
	out, err = exec.Command(zipalign, "-c", "-P", "16", "-v", "4", bad).CombinedOutput()
	if err != nil {
		t.Fatalf("zipalign was expected to EXIT ZERO on a compressed library, which is "+
			"the whole finding; it failed instead: %v\n%s", err, out)
	}
	if line := libLine(string(out)); !strings.Contains(line, "compressed") {
		t.Errorf("the control APK's library line is %q, want it to say compressed — "+
			"without that this test is not measuring what it claims", line)
	}
}

// libLine returns the verbose line about a .so entry.
func libLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, ".so") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// findZipalign locates the SDK's zipalign, skipping when there is none: this
// test needs the real tool, and a machine without an SDK cannot run it.
func findZipalign(t *testing.T) string {
	t.Helper()
	home := os.Getenv("ANDROID_HOME")
	if home == "" {
		home = os.Getenv("ANDROID_SDK_ROOT")
	}
	if home == "" {
		for _, c := range []string{"/Users/Shared/android-sdk", "/usr/local/lib/android/sdk"} {
			if _, err := os.Stat(filepath.Join(c, "build-tools")); err == nil {
				home = c
				break
			}
		}
	}
	if home == "" {
		t.Skip("no Android SDK on this machine: ANDROID_HOME is unset and none of the " +
			"usual directories holds build-tools, so the SDK's own verdict cannot be asked for")
	}
	sdk, err := FindSDK(home)
	if err != nil {
		t.Skipf("no usable SDK under %s: %v", home, err)
	}
	p := sdk.Tool("zipalign")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("build-tools %s has no zipalign", sdk.BuildTools)
	}
	return p
}

// TestZipalignAgreesReadsTheOutputNotTheStatus drives the reader with the
// answers the real tool gives, so the three it must distinguish are all
// covered without an SDK.
func TestZipalignAgreesReadsTheOutputNotTheStatus(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		out     string
		wantErr string
	}{
		{
			name: "a stored and aligned library passes",
			out: "Verifying alignment of out.apk (4)...\n" +
				"      49 classes.dex (OK - compressed)\n" +
				"   16384 lib/arm64-v8a/libxrapp.so (OK)\n" +
				"Verification succesful\n",
		},
		{
			name: "a compressed library is refused although the status is zero",
			out: "Verifying alignment of out.apk (4)...\n" +
				"   35831 lib/arm64-v8a/libxrapp.so (OK - compressed)\n" +
				"Verification succesful\n",
			wantErr: "did not apply",
		},
		{
			name: "a misaligned library is refused",
			out: "Verifying alignment of out.apk (4)...\n" +
				"   35830 lib/arm64-v8a/libxrapp.so (BAD - 2)\n",
			wantErr: "BAD",
		},
		{
			name:    "no library listed at all says nothing about the payload",
			out:     "Verifying alignment of out.apk (4)...\nVerification succesful\n",
			wantErr: "listed no .so entry",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			run := funcRunner(func(context.Context, string, []string, string, ...string) ([]byte, error) {
				return []byte(c.out), nil
			})
			err := zipalignAgrees(context.Background(), run, ".", nil,
				SDK{Home: "h", BuildTools: "35.0.0"}, "out.apk", nil)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("refused a good answer: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %q", c.out)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("refusal %q does not carry %q", err, c.wantErr)
			}
		})
	}
	// And a zipalign that itself fails is reported as such rather than read.
	run := funcRunner(func(context.Context, string, []string, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	})
	if err := zipalignAgrees(context.Background(), run, ".", nil, SDK{}, "out.apk", nil); err == nil {
		t.Error("a zipalign that could not run was treated as a pass")
	}
}

// funcRunner adapts a function to Runner.
type funcRunner func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

func (f funcRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	return f(ctx, dir, env, name, args...)
}
