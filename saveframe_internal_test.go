// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// ⚠ THE ENCODER'S ERROR PATH, THROUGH ITS SEAM, for the same reason as getwd's
// and abs's next door: [SaveFrame] encodes into a bytes.Buffer, whose Write
// never fails, so the check cannot be reached with the real png.Encode.
// Dropping the error instead of checking it is how Close lost a reconnect in
// this package once already — so the check stays and the seam makes it
// testable.
func TestSaveFrameReportsAnEncoderThatFails(t *testing.T) {
	boom := errors.New("the encoder said no")
	old := encodePNG
	t.Cleanup(func() { encodePNG = old })
	encodePNG = func(io.Writer, image.Image) error { return boom }

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path, err := SaveFrame("frame-001.png", image.NewRGBA(image.Rect(0, 0, 4, 4)))
	if !errors.Is(err, boom) {
		t.Fatalf("SaveFrame = %q, %v; want the encoder's error", path, err)
	}
	// ⛔ AND NOTHING WAS LEFT BEHIND. A half-written frame on disk reads to the
	// replay tool as a corrupt picture, which is a different complaint from "it
	// was never written" — and encoding into memory first is what makes that
	// true rather than hoped for.
	if _, err := os.Stat(filepath.Join(dir, "frame-001.png")); !os.IsNotExist(err) {
		t.Errorf("a file was left behind after the encoder failed: %v", err)
	}
}

// ⚠ AND THE SEAM IS THE REAL CALL BY DEFAULT, which is the half that stops the
// test above from proving a fiction: with nothing replaced, a frame becomes a
// PNG that image/png itself reads back.
func TestUnreplacedTheEncoderIsPngEncode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path, err := SaveFrame("frame-002.png", image.NewRGBA(image.Rect(0, 0, 8, 4)))
	if err != nil {
		t.Fatalf("SaveFrame: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The PNG signature, from the format's own specification. Checked as bytes
	// rather than by decoding, because decoding is what the exported test next
	// door does and two tests asserting the same thing the same way is one test.
	want := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if len(b) < len(want) {
		t.Fatalf("the file is %d bytes, too short to be a PNG", len(b))
	}
	for i, v := range want {
		if b[i] != v {
			t.Fatalf("byte %d of the file is %#02x, want %#02x: it is not a PNG",
				i, b[i], v)
		}
	}
}
