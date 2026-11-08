// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeTinyZip writes the smallest archive shaped like what aapt2 link
// produces, so the fuzzer starts from something valid and mutates away from it.
func writeTinyZip(w *bytes.Buffer) {
	z := zip.NewWriter(w)
	for _, e := range []struct {
		name   string
		method uint16
	}{
		{"AndroidManifest.xml", zip.Store},
		{"resources.arsc", zip.Store},
	} {
		hw, err := z.CreateHeader(&zip.FileHeader{Name: e.name, Method: e.method})
		if err != nil {
			return
		}
		if _, err := hw.Write([]byte("some bytes")); err != nil {
			return
		}
	}
	_ = z.Close()
}

func writeFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}

// FuzzStripAlignExtra drives the only parser this package has.
//
// ⛔ ITS INPUT COMES OUT OF A ZIP FILE. stripAlignExtra walks the extra-field
// list of an entry aapt2 wrote, and "aapt2 wrote it" is a claim about the happy
// path: the file is on disk between two tools, a build can be interrupted
// halfway through writing it, and the field list is (id, size) pairs where the
// size is attacker-chosen if anybody can place a file where the packer reads
// one. A walk that trusts the size runs off the end of the slice.
//
// ⚠ AND IT ASSERTS PROPERTIES, not just the absence of a panic:
//
//   - the output is never longer than the input, because the function only
//     ever DROPS fields;
//   - it is idempotent, because stripping padding from something with no
//     padding must change nothing -- that is what makes it safe to call on an
//     entry that has already been through it;
//   - and no well-formed padding field survives, which is the thing it is for.
func FuzzStripAlignExtra(f *testing.F) {
	field := func(id uint16, payload []byte) []byte {
		b := make([]byte, 4+len(payload))
		binary.LittleEndian.PutUint16(b[0:2], id)
		binary.LittleEndian.PutUint16(b[2:4], uint16(len(payload)))
		copy(b[4:], payload)
		return b
	}
	f.Add([]byte{})
	f.Add(field(alignExtraID, make([]byte, 12)))
	f.Add(field(0x5455, []byte{1, 0x35, 0xd9, 0, 0}))
	f.Add(append(field(0x5455, []byte{1, 2, 3, 4, 5}), field(alignExtraID, make([]byte, 8))...))
	f.Add([]byte{0x35, 0xd9})                   // a truncated field header
	f.Add([]byte{0x35, 0xd9, 0xff, 0xff, 0x00}) // a size past the end

	f.Fuzz(func(t *testing.T, extra []byte) {
		got := stripAlignExtra(extra)
		if len(got) > len(extra) {
			t.Fatalf("stripAlignExtra grew %d bytes into %d: it only removes fields",
				len(extra), len(got))
		}
		if again := stripAlignExtra(got); !bytes.Equal(again, got) {
			t.Fatalf("stripAlignExtra is not idempotent:\n in  %x\n out %x\n again %x",
				extra, got, again)
		}
		// No WELL-FORMED padding field may remain. A malformed tail is kept on
		// purpose -- a parser that cannot see where a field ends cannot know
		// where the next one starts -- so the scan below stops where the
		// function's own would.
		for b := got; len(b) >= 4; {
			id := binary.LittleEndian.Uint16(b[0:2])
			size := int(binary.LittleEndian.Uint16(b[2:4]))
			if 4+size > len(b) {
				break
			}
			if id == alignExtraID {
				t.Fatalf("a padding field survived stripping:\n in  %x\n out %x", extra, got)
			}
			b = b[4+size:]
		}
	})
}

// FuzzRepackReadsWhateverIsOnDisk feeds Repack a base archive it did not write.
//
// ⛔ aapt2 WRITES THAT FILE AND THIS CODE READS IT, which is a boundary however
// friendly the two tools are. A build killed between the two leaves a truncated
// zip; a packer that panics on one turns a tidy "try again" into a stack trace,
// and a packer that writes an APK from a half-read archive signs something
// nobody inspected.
func FuzzRepackReadsWhateverIsOnDisk(f *testing.F) {
	var good bytes.Buffer
	writeTinyZip(&good)
	f.Add(good.Bytes())
	f.Add(good.Bytes()[:len(good.Bytes())/2]) // truncated
	f.Add([]byte("PK\x03\x04 and then nothing"))
	f.Add([]byte{})

	// ⚠ ONE DIRECTORY FOR THE WHOLE TARGET, NOT ONE PER INPUT. With t.TempDir()
	// inside the body this target managed six executions a second, while the
	// same calls run about a thousand a second from an ordinary test -- so the
	// cost was the directory churn and not Repack. A fuzz target that cannot
	// get through its inputs is a gate that reports success for having tried
	// almost nothing, which is this repository's recurring defect wearing
	// another hat.
	dir := f.TempDir()
	src := filepath.Join(dir, "base.apk")
	out := filepath.Join(dir, "out.apk")

	f.Fuzz(func(t *testing.T, base []byte) {
		if err := writeFile(src, base); err != nil {
			t.Skip("could not write the fixture, which is not what is under test")
		}
		_ = os.Remove(out)
		err := Repack(src, out, []Add{
			{Name: "lib/arm64-v8a/libxrapp.so", Data: []byte{0x7f, 'E', 'L', 'F'}, Store: true},
		})
		if err != nil {
			return
		}
		// ⛔ IF IT CLAIMED SUCCESS, THE RESULT MUST BE AN APK THIS PACKAGE WOULD
		// ACCEPT. Succeeding on a damaged base and writing something only
		// apksigner will complain about is worse than refusing.
		if err := Verify(out); err != nil {
			t.Fatalf("Repack succeeded on a %d-byte base and wrote an APK Verify "+
				"refuses: %v", len(base), err)
		}
	})
}
