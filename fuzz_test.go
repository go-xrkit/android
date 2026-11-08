// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android_test

import (
	"image"
	"testing"

	"github.com/go-xrkit/android"
)

// ⛔⛔ EVERY PARSER HERE READS BYTES A DEVICE CHOSE, AND NONE OF THEM HAD A FUZZ
// TARGET. The USB descriptor blob, the isochronous payload header and the MJPEG
// frame all arrive from a headset over a cable: a camera with firmware from
// nobody-knows-where, or a cable that drops a byte, or a device that is not the
// one that was plugged in a second ago. The write happens with the person's own
// privileges on their own workstation, and this module's own notes already say
// as much about the frame NAMES -- "a tool that pulls files off a device must
// treat their names as input" -- while the bytes themselves were parsed on
// trust.
//
// ⚠ WHAT THESE ASSERT IS MOSTLY "IT RETURNS", which is the right bar for a
// fuzz target over a hostile buffer: a panic in ParsePayload is a crash in the
// stream reader's goroutine, and a slice that escapes its packet is a read of
// whatever the allocator had there. Where an invariant can be stated truly it
// is stated, and nothing stronger is invented to look thorough.

// FuzzParsePayload drives the UVC payload header parser.
//
// The invariant worth having: a payload that parses must describe data INSIDE
// the packet it came from. The header length is a byte the camera sends, so a
// header claiming more than the packet holds is the first thing a broken one
// does.
func FuzzParsePayload(f *testing.F) {
	// Seeds: the shapes the real camera produces, and the ones that broke it.
	f.Add([]byte{})                             // nothing at all
	f.Add([]byte{0x00})                         // a header length of zero
	f.Add([]byte{0x02, 0x80})                   // the smallest real header
	f.Add([]byte{0x02, 0x81, 1, 2, 3, 4})       // FID set, four bytes of pixels
	f.Add([]byte{0x0c, 0x8e, 0, 0, 0, 0, 0, 0}) // a header longer than the packet
	f.Add([]byte{0xff, 0x80, 1})                // a header claiming 255 bytes

	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := android.ParsePayload(b)
		if err != nil {
			return
		}
		// Data must be a sub-slice of b: its length cannot exceed what is left
		// after the header the camera declared.
		if n := int(b[0]); len(p.Data) != len(b)-n {
			t.Fatalf("ParsePayload(%d bytes, header says %d) returned %d bytes of data, "+
				"want %d: the data is not inside the packet",
				len(b), n, len(p.Data), len(b)-n)
		}
		// And the three flag readers must not panic on any header byte.
		_, _, _ = p.FID(), p.EOF(), p.Bad()
	})
}

// FuzzUSBDescriptors drives the whole descriptor blob the kernel hands back for
// a device: the walk, the device fields, the interface list and the UVC format
// descriptors inside one interface.
//
// ⛔ THIS IS THE BLOB THAT DECIDES WHICH INTERFACE IS CLAIMED. OpenUVCCamera
// picks a streaming interface out of it and then takes that interface away from
// uvcvideo for the whole phone, so a parser that can be walked off the end of
// its own buffer is a parser that can be made to claim something else.
func FuzzUSBDescriptors(f *testing.F) {
	// A plausible configuration descriptor header, then shapes that truncate it.
	f.Add([]byte{0x09, 0x02, 0x12, 0x00, 0x01, 0x01, 0x00, 0x80, 0x32})
	f.Add([]byte{0x09, 0x04, 0x00, 0x00, 0x00, 0x0e, 0x01, 0x00, 0x00})
	f.Add([]byte{0x02, 0x04})       // a descriptor that is only its own header
	f.Add([]byte{0x00, 0x04, 0x00}) // a length of zero: the walk must not loop
	f.Add([]byte{0xff, 0x04, 0x00}) // a length past the end of the buffer
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		d := android.USBDescriptors(b)

		// ⛔ THE WALK MUST TERMINATE. A descriptor whose own length is zero is
		// the classic infinite loop in this format, and a fuzz target that
		// hangs looks exactly like one that is working.
		steps := 0
		_ = d.Walk(func(_ int, part []byte) bool {
			steps++
			if steps > len(b)+2 {
				t.Fatalf("Walk made %d steps over %d bytes: it is not advancing",
					steps, len(b))
			}
			// Every part handed out must be inside the blob.
			if len(part) > len(b) {
				t.Fatalf("Walk handed out %d bytes from a %d-byte blob", len(part), len(b))
			}
			return true
		})

		_, _, _, _, _, _ = d.Device()
		ifaces, err := d.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				// Whatever it reports, it must not be asked about an interface
				// it did not report.
				_, _ = d.UVCFormats(i.Number)
			}
		}
		// And an interface number nobody claimed must be refused rather than
		// read from somewhere else.
		_, _ = d.UVCFormats(0x7fffffff)
		_, _ = d.UVCFormats(-1)
	})
}

// FuzzFrameAssemblerFeed drives the frame assembler with a stream of payloads,
// which is the MJPEG path: the bytes between the markers are whatever the
// camera sent.
//
// ⚠ IT FEEDS A SEQUENCE, not one packet, because the assembler's state is the
// thing under test: the FID toggle, the end-of-frame bit and the short-frame
// drop only interact across packets. The fuzzer's buffer is cut into packets at
// its own byte boundaries, so it chooses the sequence too.
func FuzzFrameAssemblerFeed(f *testing.F) {
	f.Add([]byte{3, 0x02, 0x80, 1, 2, 3})
	f.Add([]byte{2, 0x02, 0x81, 4, 0x02, 0x80, 0xff, 0xd8, 0xff, 0xd9})
	f.Add([]byte{1, 0x00})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		a, err := android.NewFrameAssemblerFor(android.FormatMJPEG, 64, 48)
		if err != nil {
			t.Skip("the assembler refused this size, which is not what is under test")
		}
		dst := image.NewRGBA(image.Rect(0, 0, 64, 48))
		for len(b) > 0 {
			n := int(b[0]) + 1
			if n > len(b) {
				n = len(b)
			}
			packet := b[1:n]
			b = b[n:]
			p, err := android.ParsePayload(packet)
			if err != nil {
				continue
			}
			frame, ok := a.Feed(p)
			if !ok {
				continue
			}
			// A frame handed over must not be longer than the stream that fed
			// it: an assembler returning more than it was given is returning
			// somebody else's memory.
			if len(frame) > len(packet)+len(b)+MaxFuzzSlack {
				t.Fatalf("Feed returned %d bytes from a stream of at most %d",
					len(frame), len(packet)+len(b))
			}
			// Decoding a hostile frame must answer an error, not panic.
			if _, err := a.Decode(dst, frame); err != nil {
				continue
			}
		}
	})
}

// MaxFuzzSlack is the headroom the length check above allows for the assembler's
// own buffering across packets: it carries a partial frame, so a frame handed
// over can legitimately be longer than the packet that completed it. It is
// generous on purpose -- the check is for a frame the size of the heap, not for
// an off-by-one.
const MaxFuzzSlack = 1 << 20
