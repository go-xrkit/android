// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android_test

import (
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-xrkit/android"
)

// bars paints a frame with structure down its columns, as a camera's decoder
// hands one over: the luminance already in all three channels.
func bars(w, h int) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		row := im.Pix[y*im.Stride:]
		for x := 0; x < w; x++ {
			v := byte(0x20)
			if (x/8)%2 == 0 {
				v = 0xD0
			}
			i := x * 4
			row[i], row[i+1], row[i+2], row[i+3] = v, v, v, 0xFF
		}
	}
	return im
}

// TestSaveFrameWritesAPNGAPERSONANDATRACKERCANBOTHREAD.
//
// ⭐ PNG RATHER THAN A FORMAT OF OUR OWN, and the reason is structural: these
// frames are fed to go-xrkit/xrkit/headflow, which lives in a module whose
// dependency tree this one has no business acquiring. A file is the interface,
// image/png is in the standard library on both sides, and the two halves share
// no code at all — so this test decodes with the same package the reader uses
// rather than with a parser of ours that could drift from it.
func TestSaveFrameWritesAPNGAPersonAndATrackerCanBothRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const w, h = 64, 16

	path, err := android.SaveFrame("frame-007.png", bars(w, h))
	if err != nil {
		t.Fatalf("SaveFrame: %v", err)
	}
	if got, want := filepath.Dir(path), home; !sameDir(t, got, want) {
		t.Fatalf("the frame landed in %q, want the host's HOME %q", got, want)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening it back: %v", err)
	}
	defer func() { _ = f.Close() }()
	im, err := png.Decode(f)
	if err != nil {
		t.Fatalf("the file is not a PNG: %v", err)
	}
	if b := im.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("the PNG is %v, want %dx%d", b, w, h)
	}
	// ⚠ GREY, BECAUSE GREY IS WHAT IS READ: headflow.Profile sums R+G+B down
	// each column and every decoder here already writes the luminance into all
	// three channels, so storing one plane loses nothing a tracker uses and is a
	// quarter of the size.
	g, ok := im.(*image.Gray)
	if !ok {
		t.Fatalf("the PNG decoded to %T, want *image.Gray", im)
	}
	// ⛔ AND THE BARS SURVIVED. A uniform image is what a broken write looks
	// like to a decoder that only checks the size — the same trap as a black
	// frame looking like a working stream to a counter.
	var dark, light int
	for _, v := range g.Pix {
		if v < 0x80 {
			dark++
		} else {
			light++
		}
	}
	if dark == 0 || light == 0 {
		t.Fatalf("%d dark and %d light pixels: the structure did not survive", dark, light)
	}
}

// ⛔⛔ A FRAME IS A PICTURE OF WHATEVER THE CAMERA WAS POINTED AT, which on a
// headset is a room and the people in it. The rule this package already enforces
// for a transcript's fallback directory governs frames too, and it is measured
// here rather than assumed — including when somebody points the override at
// their own work tree.
func TestSaveFrameRefusesAWorkTree(t *testing.T) {
	tree := t.TempDir()
	if err := os.Mkdir(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	t.Setenv(android.ArtifactEnv, tree)
	if _, err := android.SaveFrame("frame-001.png", bars(8, 8)); !errors.Is(err,
		android.ErrInRepository) {
		t.Fatalf("SaveFrame into a work tree = %v, want ErrInRepository", err)
	}
}

func TestSaveFrameRefusesWhatItCannotWrite(t *testing.T) {
	t.Run("no frame at all", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if _, err := android.SaveFrame("frame-001.png", nil); !errors.Is(err,
			android.ErrInvalidOption) {
			t.Fatalf("SaveFrame(nil) = %v, want ErrInvalidOption", err)
		}
	})
	// ⛔⛔ A DIRECTORY THAT IS NOT THERE, NOT ONE WITH NO PERMISSIONS. The first
	// version chmod'ed a directory to 0, which cost two lanes: Windows has no
	// POSIX permission bits, so the write SUCCEEDED and the test failed there;
	// and root ignores the mode, so the test skipped itself inside a container
	// and silently took the coverage with it. A missing directory fails on every
	// platform and for every user, with no permission model involved at all.
	// TestSaveTranscriptReportsADirectoryItCannotWriteTo next door already did
	// it this way, and reading it first would have saved both.
	t.Run("a directory that is not there", func(t *testing.T) {
		t.Setenv("HOME", filepath.Join(t.TempDir(), "not-there"))
		if _, err := android.SaveFrame("frame-001.png", bars(8, 8)); err == nil {
			t.Fatal("a frame was reported as written into a directory that is not there")
		}
	})
}
