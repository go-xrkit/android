// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRepackOnEveryTruncationOfAValidArchive is the deterministic half of
// FuzzRepackReadsWhateverIsOnDisk: it takes a valid archive apart one byte at a
// time and asserts the same two things about every prefix.
//
// ⛔ A BUILD CAN BE KILLED BETWEEN aapt2 AND THIS CODE, which leaves exactly
// these files on disk. The fuzzer explores further but it explores at random;
// this covers the whole of one axis, and it is what showed the slow fuzz target
// was slow rather than stuck -- 293 archives in under a second here against six
// a second under the fuzzing harness, which located the cost in a per-input
// t.TempDir() and not in Repack.
func TestRepackOnEveryTruncationOfAValidArchive(t *testing.T) {
	t.Parallel()
	var good bytes.Buffer
	writeTinyZip(&good)
	whole := good.Bytes()

	dir := t.TempDir()
	src := filepath.Join(dir, "base.apk")
	out := filepath.Join(dir, "out.apk")
	var succeeded int

	for n := 0; n <= len(whole); n++ {
		if err := os.WriteFile(src, whole[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(out)

		done := make(chan error, 1)
		go func() {
			done <- Repack(src, out, []Add{
				{Name: "lib/arm64-v8a/libxrapp.so", Data: []byte{0x7f, 'E', 'L', 'F'}, Store: true},
			})
		}()
		select {
		case err := <-done:
			if err != nil {
				continue
			}
			succeeded++
			// ⛔ AND WHEN IT SAYS YES, THE APK MUST BE ONE THIS PACKAGE WOULD
			// ACCEPT. Succeeding on a damaged base and leaving the alignment to
			// whoever checks next is the defect the fuzzer found: the offset
			// used to advance by the size the central directory CLAIMED rather
			// than by what could be read.
			if err := Verify(out); err != nil {
				t.Errorf("Repack succeeded on the first %d bytes of a %d-byte archive and "+
					"wrote an APK Verify refuses: %v", n, len(whole), err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Repack did not return within ten seconds on the first %d bytes", n)
		}
	}
	// The positive control: if nothing ever succeeded, the Verify assertion
	// above examined nothing and this test would pass on a Repack that always
	// refuses.
	if succeeded == 0 {
		t.Error("no prefix was packaged at all, so the check on the result never ran")
	}
	t.Logf("%d of %d prefixes packaged, every one of them accepted by Verify",
		succeeded, len(whole)+1)
}
