// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import "testing"

// beastAlternates is what the Beast's camera actually advertises on interface 1,
// read off the phone on 2026-10-11: alternate 0 with no endpoint, then
// increasing isochronous sizes. The sizes are wMaxPacketSize as the descriptors
// carry it — size in bits 10:0, additional transactions per microframe in
// bits 12:11 — so they are decoded, never used raw.
// ⛔⛔ THE RAW FIELD IS BUILT FROM ITS TWO HALVES, NOT TYPED AS A TOTAL. The
// first version of this fixture wrote MaxPacketSize: 2400 meaning "2400 bytes an
// interval" — but 2400 as a raw wMaxPacketSize decodes to 352 x 2 = 704, so the
// test demanded something the fixture did not describe. Writing (size,
// transactions) and letting the decode produce the total is the only way the
// fixture cannot disagree with itself.
func beastAlternates() []USBInterface {
	iso := func(size, extra int) []USBEndpoint {
		return []USBEndpoint{{
			Address: 0x81, Attributes: 1,
			MaxPacketSize: size | extra<<11, Interval: 1,
		}}
	}
	return []USBInterface{
		// Alternate 0 carries NO endpoint. That is what zero bandwidth means.
		{Number: 1, Alternate: 0, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming},
		{Number: 1, Alternate: 1, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
			Endpoints: iso(128, 0)}, // 128
		{Number: 1, Alternate: 2, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
			Endpoints: iso(512, 0)}, // 512
		{Number: 1, Alternate: 5, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
			Endpoints: iso(1200, 1)}, // 1200 x 2 = 2400, the device's committed payload
		{Number: 1, Alternate: 6, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
			Endpoints: iso(1024, 2)}, // 1024 x 3 = 3072, raw 0x1400
		// Another interface entirely, which must never be chosen.
		{Number: 2, Alternate: 1, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
			Endpoints: iso(1200, 1)},
	}
}

// TestTheAlternateChosenIsTheSMALLESTTHATFITS.
//
// ⛔⛔ THE PROBE TOOK THE LARGEST AND THAT WAS THE WRONG END. An alternate's
// endpoint reserves bus bandwidth for as long as it is selected, so taking 3072
// bytes an interval to carry a committed 2400 holds part of every high-speed
// microframe away from everything else for nothing. On the device the committed
// payload was exactly 2400 and alternate 5 offers exactly 2400.
func TestTheAlternateChosenIsTheSmallestThatFits(t *testing.T) {
	for _, c := range []struct {
		payload uint32
		wantAlt int
		wantEP  int
	}{
		{2400, 5, 2400}, // the device's own case: an exact fit
		{2401, 6, 3072}, // one byte more and only the big one will do
		{512, 2, 512},   // and it does not always answer 5
		{1, 1, 128},     // the smallest of all
		{3072, 6, 3072}, // exactly the largest
	} {
		alt, ep, ok := PickAlternate(beastAlternates(), 1, c.payload)
		if !ok {
			t.Errorf("PickAlternate for a %d-byte payload found nothing", c.payload)
			continue
		}
		if alt.Alternate != c.wantAlt || ep.ISOPacketSize() != c.wantEP {
			t.Errorf("a %d-byte payload chose alternate %d (%d bytes an interval), "+
				"want alternate %d (%d)", c.payload, alt.Alternate,
				ep.ISOPacketSize(), c.wantAlt, c.wantEP)
		}
		if alt.Number != 1 {
			t.Errorf("a %d-byte payload chose interface %d", c.payload, alt.Number)
		}
	}
}

func TestAnAlternateIsRefusedRatherThanApproximated(t *testing.T) {
	// More than the largest alternate carries: there is no answer, and saying
	// so beats selecting something that cannot hold a frame's payload.
	if _, _, ok := PickAlternate(beastAlternates(), 1, 3073); ok {
		t.Error("a payload larger than every alternate was given one anyway")
	}
	// An interface that does not exist.
	if _, _, ok := PickAlternate(beastAlternates(), 9, 100); ok {
		t.Error("an interface that does not exist was given an alternate")
	}
	// Nothing at all.
	if _, _, ok := PickAlternate(nil, 1, 100); ok {
		t.Error("an empty descriptor set produced an alternate")
	}
}

// TestZeroBandwidthIsNeverCHOSEN.
//
// ⛔ ALTERNATE 0 CARRIES NO ENDPOINT, so it can never serve a transfer — but a
// camera that advertised one there would still be wrong to pick, because UVC
// takes the move AWAY from zero bandwidth as the signal to start sending. It is
// excluded by number, not by whether it happens to have an endpoint.
func TestZeroBandwidthIsNeverChosen(t *testing.T) {
	ifaces := beastAlternates()
	// Give alternate 0 an endpoint big enough to win on size alone.
	ifaces[0].Endpoints = []USBEndpoint{
		{Address: 0x81, Attributes: 1, MaxPacketSize: 64, Interval: 1},
	}
	alt, _, ok := PickAlternate(ifaces, 1, 16)
	if !ok {
		t.Fatal("PickAlternate found nothing")
	}
	if alt.Alternate == ZeroBandwidthAlternate {
		t.Error("alternate 0 was chosen; the move away from it is what starts a " +
			"UVC stream, so streaming on it cannot work however it is described")
	}
}

// An endpoint that is not an isochronous IN is not a streaming endpoint, whatever
// its size.
func TestOnlyAnIsochronousInEndpointIsConsidered(t *testing.T) {
	ifaces := []USBInterface{
		{Number: 1, Alternate: 1, Endpoints: []USBEndpoint{
			{Address: 0x01, Attributes: 1, MaxPacketSize: 2400}, // OUT
			{Address: 0x82, Attributes: 2, MaxPacketSize: 2400}, // bulk IN
			{Address: 0x83, Attributes: 3, MaxPacketSize: 2400}, // interrupt IN
			{Address: 0x84, Attributes: 1, MaxPacketSize: 0},    // no size at all
		}},
	}
	if _, _, ok := PickAlternate(ifaces, 1, 100); ok {
		t.Error("an alternate with no isochronous IN endpoint was chosen")
	}
	// And the same interface with one added is chosen, so the refusal above is
	// about the endpoints and not about the interface being unreachable.
	ifaces[0].Endpoints = append(ifaces[0].Endpoints,
		USBEndpoint{Address: 0x81, Attributes: 1, MaxPacketSize: 2400})
	if _, ep, ok := PickAlternate(ifaces, 1, 100); !ok || ep.Address != 0x81 {
		t.Errorf("the isochronous IN endpoint was not chosen (ok=%v, ep=%#02x)",
			ok, ep.Address)
	}
}
