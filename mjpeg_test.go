// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegFrame encodes a real JPEG of the given size, with structure down its
// columns so a luminance profile of it is not flat.
//
// ⛔ A REAL ENCODER, NOT A FIXTURE. A hand-written byte string that happened to
// start FF D8 and end FF D9 would pass the marker test and tell us nothing about
// decoding — and this package has already shipped one test whose expectations
// were written by hand and were all wrong. image/jpeg is in the standard
// library, so the oracle costs nothing.
func jpegFrame(t testing.TB, w, h int, quality int) []byte {
	return encodeJPEG(t, w, h, quality, true)
}

// encodeJPEG encodes a real JPEG, in colour or grey.
//
// ⛔⛔ THE COLOUR ONE IS NOT A LUXURY, IT IS THE PATH A CAMERA TAKES. An
// *image.Gray encodes to a GRAYSCALE JPEG, which image/jpeg decodes back to
// *image.Gray — so a suite whose every fixture was grey exercised the fallback
// and never once touched the YCbCr loop that a colour camera's frames go
// through. The coverage gate found that, and it is exactly the shape of a test
// that agrees with the code about a case neither has seen.
func encodeJPEG(t testing.TB, w, h, quality int, colour bool) []byte {
	t.Helper()
	var src image.Image
	if colour {
		im := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				// Vertical bars, and a colour that varies down the frame so the
				// chrominance planes are not constant either.
				v := uint8(0x20)
				if (x/16)%2 == 0 {
					v = 0xD0
				}
				im.Set(x, y, color.RGBA{R: v, G: v, B: uint8(y * 255 / h), A: 0xFF})
			}
		}
		src = im
	} else {
		im := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := byte(0x20)
				if (x/16)%2 == 0 {
					v = 0xD0
				}
				im.SetGray(x, y, color.Gray{Y: v})
			}
		}
		src = im
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, src, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encoding a %dx%d JPEG: %v", w, h, err)
	}
	return out.Bytes()
}

// TestAWholeJPEGIsRECOGNISEDBYITSMARKERS.
//
// ⛔⛔ A LENGTH TEST IS MEANINGLESS ON A COMPRESSED FRAME. A JPEG of a plain wall
// is a fraction of the size of one of a bookshelf, so "shorter than expected"
// describes a scene rather than a loss — which is why the YUY2 rule cannot be
// reused here. What a lost tail looks like is a valid beginning and no end, and
// that is exactly what the markers catch.
func TestAWholeJPEGIsRecognisedByItsMarkers(t *testing.T) {
	t.Parallel()
	whole := jpegFrame(t, 320, 240, 80)
	if !wholeJPEG(whole) {
		t.Fatal("a JPEG straight from the encoder was not recognised as whole")
	}
	// ⭐ THE SAME PICTURE AT TWO QUALITIES HAS TWO LENGTHS, which is the fact
	// that makes a length test impossible. If these ever matched, this test
	// would be proving nothing.
	coarse := jpegFrame(t, 320, 240, 20)
	if len(coarse) == len(whole) {
		t.Fatalf("two qualities gave the same %d bytes; the fixture no longer "+
			"demonstrates that length says nothing", len(whole))
	}
	if !wholeJPEG(coarse) {
		t.Error("the coarser JPEG was not recognised as whole")
	}

	for _, c := range []struct {
		name string
		in   []byte
	}{
		{"a lost tail: the end-of-image marker never arrived",
			whole[:len(whole)-2]},
		{"a lost head: the packets before the start were dropped",
			whole[2:]},
		{"nothing at all", nil},
		{"the markers and nothing between them",
			append(append([]byte{}, jpegSOI...), jpegEOI...)},
		{"YUY2 bytes offered as a JPEG",
			bytes.Repeat([]byte{0x80, 0x80}, 128)},
	} {
		if wholeJPEG(c.in) {
			t.Errorf("%s: accepted as a whole JPEG", c.name)
		}
	}
}

func TestAnMJPEGFrameDecodesToLuminance(t *testing.T) {
	t.Parallel()
	const w, h = 320, 240
	a, err := NewFrameAssemblerFor(FormatMJPEG, w, h)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Format(); got != FormatMJPEG {
		t.Errorf("Format = %s, want MJPEG", got)
	}
	img, err := a.Decode(nil, jpegFrame(t, w, h, 90))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if img.Rect.Dx() != w || img.Rect.Dy() != h {
		t.Fatalf("Decode gave a %v image, want %dx%d", img.Rect, w, h)
	}
	// ⭐ THE Y PLANE GOES INTO ALL THREE CHANNELS, as it does for YUY2 and for
	// the same reason: headflow.Profile sums R+G+B down each column, so a
	// YCbCr-to-RGB conversion would compute chrominance for the consumer to add
	// up and throw away.
	var dark, light int
	for i := 0; i < len(img.Pix); i += 4 {
		r, g, b, al := img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]
		if r != g || g != b {
			t.Fatalf("pixel %d is %d,%d,%d — not grey, so a colour conversion "+
				"happened that nothing reads", i/4, r, g, b)
		}
		if al != 0xFF {
			t.Fatalf("pixel %d has alpha %d, want opaque", i/4, al)
		}
		if r < 0x60 {
			dark++
		} else {
			light++
		}
	}
	// ⛔⛔ AND THE BARS SURVIVED, because a uniformly grey image is exactly what
	// a broken decode looks like to a counter — the same trap as a black frame.
	if dark == 0 || light == 0 {
		t.Fatalf("the decoded frame has %d dark and %d light pixels; the vertical "+
			"bars did not survive, so this is not the picture that was encoded",
			dark, light)
	}
}

// TestAGrayscaleJPEGISALSOAPICTURE.
//
// ⛔ A CAMERA MAY SEND ONE, and image/jpeg decodes it to *image.Gray rather than
// *image.YCbCr. Refusing it would turn a working camera into a dead one over a
// detail nothing here cares about — the luminance is all this path reads either
// way. Both shapes are tested because for a while only this one was, and the
// YCbCr loop that a colour camera actually uses was covered by nothing.
func TestAGrayscaleJPEGIsAlsoAPicture(t *testing.T) {
	t.Parallel()
	const w, h = 320, 240
	a, err := NewFrameAssemblerFor(FormatMJPEG, w, h)
	if err != nil {
		t.Fatal(err)
	}
	img, err := a.Decode(nil, encodeJPEG(t, w, h, 90, false))
	if err != nil {
		t.Fatalf("Decode of a grayscale JPEG: %v", err)
	}
	var dark, light int
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] != img.Pix[i+1] || img.Pix[i+1] != img.Pix[i+2] {
			t.Fatalf("pixel %d is not grey", i/4)
		}
		if img.Pix[i] < 0x60 {
			dark++
		} else {
			light++
		}
	}
	if dark == 0 || light == 0 {
		t.Fatalf("%d dark and %d light: the bars did not survive", dark, light)
	}
}

// TestAnMJPEGFrameEndedByEOFWhileINCOMPLETEIsAttributedToIt: the mirror of the
// toggle case, so the per-boundary census means something for compressed frames
// too.
func TestAnMJPEGFrameEndedByEOFWhileIncompleteIsAttributedToIt(t *testing.T) {
	t.Parallel()
	const w, h = 320, 240
	a, err := NewFrameAssemblerFor(FormatMJPEG, w, h)
	if err != nil {
		t.Fatal(err)
	}
	frame := jpegFrame(t, w, h, 80)
	// Half a JPEG, then the camera marks it finished anyway. Both markers are
	// required and the end-of-image one never arrived.
	half := frame[:len(frame)/2]
	p, err := ParsePayload(payload(2, payloadEOF, half...))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Feed(p); ok {
		t.Fatal("half a JPEG was delivered as a frame")
	}
	onT, onE := a.Losses()
	if onT != 0 || onE != 1 {
		t.Errorf("Losses = %d on toggle, %d on EOF; want it attributed to the EOF",
			onT, onE)
	}
	// ⭐⭐ AND WHICH MARKER WAS MISSING, which is what settled the device's
	// behaviour in one run: all 67 MJPEG losses on the Beast's camera had a
	// start-of-image and no end-of-image, so the camera marks a frame finished
	// before its own picture is. Half a JPEG is that case exactly.
	noStart, noEnd := a.MissingMarkers()
	if noStart != 0 || noEnd != 1 {
		t.Errorf("MissingMarkers = %d without a start, %d without an end; half a JPEG "+
			"has its start and lost its tail", noStart, noEnd)
	}
	// ⚠ AND NOTHING IS ADDED TO THE SHORTFALL, because a compressed frame has no
	// expected length to be short OF. A zero here is honest; a guess would be a
	// number somebody averages.
	if _, _, short := a.Boundaries(); short != 0 {
		t.Errorf("the shortfall is %d bytes for a compressed frame, which has no "+
			"expected length", short)
	}
}

func TestAnMJPEGFrameThatWillNotDecodeIsRefusedAndCounted(t *testing.T) {
	t.Parallel()
	const w, h = 320, 240
	a, err := NewFrameAssemblerFor(FormatMJPEG, w, h)
	if err != nil {
		t.Fatal(err)
	}
	// ⛔ A BUFFER CAN PASS THE MARKER TEST AND STILL NOT BE A PICTURE, which is
	// why deciding whether a frame is whole and deciding whether it decodes are
	// two separate steps: the first runs inside the reap loop once a frame,
	// where milliseconds do not fit, and the second runs where they do.
	ruined := jpegFrame(t, w, h, 80)
	for i := len(jpegSOI) + 4; i < len(ruined)-len(jpegEOI)-4; i++ {
		ruined[i] = 0x00 // keep both markers, destroy everything between
	}
	if !wholeJPEG(ruined) {
		t.Fatal("the fixture lost a marker, so it no longer exercises this path")
	}
	if _, err := a.Decode(nil, ruined); !errors.Is(err, ErrBadPayload) {
		t.Errorf("Decode of a ruined JPEG = %v, want ErrBadPayload", err)
	}

	// And a JPEG of the wrong SIZE: the camera committed to one thing and sent
	// another, and indexing the destination by what we asked for would read past
	// the decoded planes.
	if _, err := a.Decode(nil, jpegFrame(t, 352, 288, 80)); !errors.Is(err, ErrBadPayload) {
		t.Errorf("Decode of a 352x288 JPEG into a 320x240 assembler = %v, "+
			"want ErrBadPayload", err)
	}
	if got := a.BadJPEG(); got != 2 {
		t.Errorf("BadJPEG = %d, want 2: a frame that arrived whole and is still not "+
			"a picture is a different fault from one that never arrived, and one "+
			"counter for both would hide whichever is rarer", got)
	}
}

// TestAnMJPEGStreamAssemblesByMarkersAndNotByLength: the whole point, end to
// end through Feed.
func TestAnMJPEGStreamAssemblesByMarkersAndNotByLength(t *testing.T) {
	t.Parallel()
	const w, h = 320, 240
	a, err := NewFrameAssemblerFor(FormatMJPEG, w, h)
	if err != nil {
		t.Fatal(err)
	}
	frame := jpegFrame(t, w, h, 80)
	// ⭐ FAR SHORTER THAN AN UNCOMPRESSED FRAME, which is the condition the YUY2
	// rule would have called "dropped".
	if len(frame) >= w*h*BytesPerPixelYUY2 {
		t.Fatalf("the fixture is %d bytes against %d uncompressed; it no longer "+
			"demonstrates the difference", len(frame), w*h*BytesPerPixelYUY2)
	}

	feed := func(b []byte, fid bool, eof bool) []byte {
		var out []byte
		const per = 1024
		for off := 0; off < len(b); off += per {
			end := min(off+per, len(b))
			var bits byte
			if fid {
				bits = payloadFID
			}
			if eof && end == len(b) {
				bits |= payloadEOF
			}
			p, err := ParsePayload(payload(2, bits, b[off:end]...))
			if err != nil {
				t.Fatal(err)
			}
			if f, ok := a.Feed(p); ok {
				out = f
			}
		}
		return out
	}

	got := feed(frame, false, true)
	if got == nil {
		t.Fatal("a whole JPEG fed in packets did not come out")
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("the reassembled frame is %d bytes and the original was %d",
			len(got), len(frame))
	}
	if f, d, _ := a.Stats(); f != 1 || d != 0 {
		t.Errorf("Stats = %d frames, %d dropped; want 1 and 0", f, d)
	}

	// Now a frame whose tail was lost: both the length and the missing EOI say
	// so, but only the markers are consulted.
	feed(frame[:len(frame)/2], true, false)
	feed(frame, false, true) // the toggle ends the truncated one
	if _, d, _ := a.Stats(); d != 1 {
		t.Errorf("a JPEG missing its tail was not dropped (dropped=%d)", d)
	}
	onT, onE := a.Losses()
	if onT != 1 || onE != 0 {
		t.Errorf("Losses = %d on toggle, %d on EOF; the truncated frame had no EOF "+
			"so the toggle is what ended it", onT, onE)
	}
}

// TestABufferThatBEGANMIDPICTUREIsToldApartFromOneThatLostItsTail.
//
// ⛔ THEY ARE OPPOSITE FAULTS and "dropped" cannot tell them apart. No
// start-of-image means a boundary fired where the camera had not finished a
// picture; a start with no end means the tail went missing. On the Beast's
// camera every MJPEG loss was the second kind, 67 of 67, which is what made the
// finding a statement about the camera rather than about this parser.
func TestABufferThatBeganMidPictureIsToldApartFromOneThatLostItsTail(t *testing.T) {
	t.Parallel()
	const w, h = 320, 240
	frame := jpegFrame(t, w, h, 80)

	t.Run("no start: the stream was joined mid-picture", func(t *testing.T) {
		a, err := NewFrameAssemblerFor(FormatMJPEG, w, h)
		if err != nil {
			t.Fatal(err)
		}
		// The second half of a JPEG: it has the end and not the beginning.
		tail := frame[len(frame)/2:]
		p, err := ParsePayload(payload(2, payloadEOF, tail...))
		if err != nil {
			t.Fatal(err)
		}
		a.Feed(p)
		noStart, noEnd := a.MissingMarkers()
		if noStart != 1 || noEnd != 0 {
			t.Errorf("MissingMarkers = %d without a start, %d without an end; this "+
				"buffer has no start-of-image", noStart, noEnd)
		}
	})

	t.Run("no end: the tail went missing", func(t *testing.T) {
		a, err := NewFrameAssemblerFor(FormatMJPEG, w, h)
		if err != nil {
			t.Fatal(err)
		}
		head := frame[:len(frame)/2]
		p, err := ParsePayload(payload(2, payloadEOF, head...))
		if err != nil {
			t.Fatal(err)
		}
		a.Feed(p)
		noStart, noEnd := a.MissingMarkers()
		if noStart != 0 || noEnd != 1 {
			t.Errorf("MissingMarkers = %d without a start, %d without an end; this "+
				"buffer has its start and lost its tail", noStart, noEnd)
		}
	})
}

func TestAFormatThisPackageCannotAssembleIsRefused(t *testing.T) {
	t.Parallel()
	// ⛔ THE ZERO VALUE IS FormatUnknown AND LANDS IN THE SAME REFUSAL, which is
	// the point: it is what an unrecognised descriptor becomes AND what a
	// negotiation built by hand carries, and in both cases nobody has said
	// whether a whole frame is a length or a pair of markers.
	if _, err := NewFrameAssemblerFor(FormatUnknown, 640, 480); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("NewFrameAssemblerFor(FormatUnknown) = %v, want ErrInvalidOption", err)
	}
	if _, err := NewFrameAssemblerFor(UVCFormat(99), 640, 480); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("NewFrameAssemblerFor(99) = %v, want ErrInvalidOption", err)
	}
	// And NewFrameAssembler still means YUY2, which is what every caller before
	// this change was getting.
	a, err := NewFrameAssembler(640, 480)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Format(); got != FormatYUY2 {
		t.Errorf("NewFrameAssembler gave a %s assembler, want YUY2", got)
	}
}
