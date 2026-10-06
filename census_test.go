// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"strings"
	"testing"
)

// The census's job is to decide a ROUTE, so these test the decision rather than
// the plumbing: which of the two Android APIs could reach a headset's camera,
// given what the phone said is there.

// beast is the UVC device a VITURE headset presents, as a composite device
// whose streaming endpoint is isochronous — which is what a UVC camera almost
// always is, and the case Android's Java USB API cannot read at all.
func beast(streamingAttrs int) USBDevice {
	return USBDevice{
		Name: "/dev/bus/usb/001/002", VendorID: VitureVendorID, ProductID: 0x1011,
		Manufacturer: "VITURE", Product: "VITURE Beast",
		Class: USBClassPerInterface,
		Interfaces: []USBInterface{
			{Number: 0, Class: USBClassVideo, Subclass: USBSubclassVideoControl,
				Endpoints: []USBEndpoint{{Address: 0x83, Attributes: 0x03, MaxPacketSize: 16, Interval: 8}}},
			{Number: 1, Alternate: 0, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming},
			{Number: 1, Alternate: 1, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
				Endpoints: []USBEndpoint{{Address: 0x81, Attributes: streamingAttrs, MaxPacketSize: 3072, Interval: 1}}},
		},
	}
}

func TestTheRouteIsChosenFromWhatThePhoneOffers(t *testing.T) {
	phoneCams := []Camera{
		{ID: "0", Facing: FacingBack, Width: 4000, Height: 3000},
		{ID: "1", Facing: FacingFront, Width: 3840, Height: 2800},
	}
	external := append(append([]Camera{}, phoneCams...),
		Camera{ID: "2", Facing: FacingExternal, Width: 1920, Height: 1080})

	for _, c := range []struct {
		name string
		cams []Camera
		devs []USBDevice
		want CameraRoute
	}{
		{"nothing at all", nil, nil, RouteNone},
		{"the phone's own cameras only", phoneCams, nil, RouteNone},
		{"an external camera2 device", external, nil, RouteCamera2},
		{"a bulk UVC device", phoneCams, []USBDevice{beast(0x02)}, RouteUSBBulk},
		{"an isochronous UVC device", phoneCams, []USBDevice{beast(0x05)}, RouteUSBIsochronous},
		// ⭐ CAMERA2 WINS WHEN BOTH ARE THERE, and not by accident: the platform
		// then does the UVC negotiation, the format conversion and the
		// buffering that the USB route means writing by hand.
		{"both", external, []USBDevice{beast(0x02)}, RouteCamera2},
		// A USB device that is not a camera decides nothing.
		{"some other USB device", phoneCams, []USBDevice{{
			Name: "/dev/bus/usb/001/003", VendorID: 0x05ac,
			Interfaces: []USBInterface{{Number: 0, Class: 0x03}},
		}}, RouteNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ChooseRoute(c.cams, c.devs); got != c.want {
				t.Fatalf("ChooseRoute = %s, want %s", got, c.want)
			}
		})
	}
}

// ⛔ ISOCHRONOUS IS NOT A USABLE ROUTE, and saying so is the point of listing
// it. UsbDeviceConnection submits control, bulk and interrupt transfers and
// nothing else, so a caller that treated every UVC device as reachable would
// write a transport against an API that cannot carry it.
func TestOnlyTwoOfTheFourRoutesCanBeTaken(t *testing.T) {
	for _, c := range []struct {
		r    CameraRoute
		want bool
	}{
		{RouteNone, false},
		{RouteCamera2, true},
		{RouteUSBBulk, true},
		{RouteUSBIsochronous, false},
	} {
		if got := c.r.Usable(); got != c.want {
			t.Fatalf("%s.Usable() = %t, want %t", c.r, got, c.want)
		}
	}
}

func TestADeviceKnowsItsOwnVideoInterfaces(t *testing.T) {
	d := beast(0x05)
	if !d.Viture() {
		t.Fatalf("%04x is VITURE's vendor id and was not recognised", d.VendorID)
	}
	if (USBDevice{VendorID: 0x1234}).Viture() {
		t.Fatal("a device that is not VITURE's was taken for one")
	}
	vs := d.VideoStreaming()
	if len(vs) != 2 {
		t.Fatalf("found %d streaming interfaces, want 2 (alt 0 and alt 1)", len(vs))
	}
	// ⛔ ALT 0 HAS NO ENDPOINTS, and that is not an oddity to filter out: UVC
	// requires the zero-bandwidth setting, and a census that dropped it would
	// be describing a device that does not exist.
	if len(vs[0].Endpoints) != 0 {
		t.Fatalf("alternate 0 has %d endpoints; UVC's zero-bandwidth setting has none",
			len(vs[0].Endpoints))
	}
	if d.BulkVideo() {
		t.Fatal("an isochronous streaming endpoint was reported as bulk")
	}
	if !beast(0x02).BulkVideo() {
		t.Fatal("a bulk streaming endpoint was not recognised")
	}
	// An OUT endpoint is not a camera delivering frames, whatever its type.
	out := beast(0x02)
	out.Interfaces[2].Endpoints[0].Address = 0x01
	if out.BulkVideo() {
		t.Fatal("an OUT endpoint was taken for a camera's frame stream")
	}
}

func TestTheCensusTypesSayWhatTheyAre(t *testing.T) {
	if got := (Camera{ID: "2", Facing: FacingExternal, Width: 1920, Height: 1080}).String(); got != `camera "2" external 1920x1080` {
		t.Fatalf("Camera.String = %q", got)
	}
	if !(Camera{Facing: FacingExternal}).External() || (Camera{Facing: FacingBack}).External() {
		t.Fatal("External() does not follow the facing")
	}
	for f, want := range map[CameraFacing]string{
		FacingFront: "front", FacingBack: "back", FacingExternal: "external",
		CameraFacing(-1): "facing(-1)",
	} {
		if got := f.String(); got != want {
			t.Fatalf("CameraFacing(%d).String = %q, want %q", int(f), got, want)
		}
	}
	for tt, want := range map[USBTransferType]string{
		USBTransferControl: "control", USBTransferIsochronous: "isochronous",
		USBTransferBulk: "bulk", USBTransferInterrupt: "interrupt",
		USBTransferType(9): "transfer(9)",
	} {
		if got := tt.String(); got != want {
			t.Fatalf("USBTransferType(%d).String = %q, want %q", int(tt), got, want)
		}
	}
	for r, want := range map[CameraRoute]string{
		RouteNone: "none", RouteCamera2: "camera2 external",
		RouteUSBBulk: "USB host, bulk UVC", RouteUSBIsochronous: "USB, isochronous UVC",
		CameraRoute(9): "route(9)",
	} {
		if got := r.String(); got != want {
			t.Fatalf("CameraRoute(%d).String = %q, want %q", int(r), got, want)
		}
	}

	e := USBEndpoint{Address: 0x81, Attributes: 0x05, MaxPacketSize: 3072, Interval: 1}
	if got := e.String(); got != "ep 0x81 in isochronous max 3072 interval 1" {
		t.Fatalf("USBEndpoint.String = %q", got)
	}
	if (USBEndpoint{Address: 0x01}).In() {
		t.Fatal("an OUT endpoint reported itself as IN")
	}
	if got := (USBEndpoint{Address: 0x01}).String(); !strings.Contains(got, " out ") {
		t.Fatalf("an OUT endpoint renders as %q", got)
	}

	// The whole device renders with its interfaces and their endpoints, because
	// a transcript nobody can read the endpoints off is a transcript that
	// cannot answer the question this census exists for.
	s := beast(0x05).String()
	for _, want := range []string{"35ca:1011", `"VITURE Beast"`, "UVC streaming", "UVC control", "isochronous"} {
		if !strings.Contains(s, want) {
			t.Fatalf("USBDevice.String does not mention %q:\n%s", want, s)
		}
	}
	if got := (USBInterface{Number: 3, Class: 0x03}).String(); strings.Contains(got, "UVC") {
		t.Fatalf("a HID interface renders as %q", got)
	}
}

// ⭐ "THE CAMERA IS CLOSED" IS NOT "THERE IS NOTHING TO READ". The Beast
// presents a CDC-ACM serial interface with bulk endpoints and HID interfaces
// with interrupt ones alongside its isochronous camera, and Android's Java USB
// API can carry both of those today. A census that could not tell "unreachable"
// from "reachable and silent" would be worth little.
func TestReadableKeepsWhatAndroidsOwnApiCanCarry(t *testing.T) {
	// The device the Pixel 11 Pro Fold actually reported: 35ca:1201.
	d := USBDevice{
		Name: "/dev/bus/usb/001/009", VendorID: VitureVendorID, ProductID: 0x1201,
		Manufacturer: "VITURE", Product: "VITURE Beast XR Glasses",
		Interfaces: []USBInterface{
			{Number: 0, Class: 0x02, Subclass: 0x02, Protocol: 0x01,
				Endpoints: []USBEndpoint{{Address: 0x84, Attributes: 0x03, MaxPacketSize: 16, Interval: 8}}},
			{Number: 1, Class: 0x0a, Endpoints: []USBEndpoint{
				{Address: 0x03, Attributes: 0x02, MaxPacketSize: 512},
				{Address: 0x83, Attributes: 0x02, MaxPacketSize: 512}}},
			{Number: 2, Class: 0x01, Subclass: 0x01, Protocol: 0x20},
			{Number: 3, Alternate: 1, Class: 0x01, Subclass: 0x02, Protocol: 0x20,
				Endpoints: []USBEndpoint{
					{Address: 0x02, Attributes: 0x05, MaxPacketSize: 2632, Interval: 4},
					{Address: 0x82, Attributes: 0x05, MaxPacketSize: 4, Interval: 8}}},
			{Number: 5, Class: 0x03, Endpoints: []USBEndpoint{
				{Address: 0x85, Attributes: 0x03, MaxPacketSize: 64, Interval: 1},
				{Address: 0x04, Attributes: 0x03, MaxPacketSize: 64, Interval: 1}}},
		},
	}
	got := d.Readable()
	if len(got) != 3 {
		t.Fatalf("Readable kept %d interfaces, want 3 (CDC control, CDC data, HID): %v", len(got), got)
	}
	for _, i := range got {
		if i.Class == 0x01 {
			t.Fatalf("an isochronous audio interface was reported as readable: %s", i)
		}
	}

	// ⛔ AN OUT ENDPOINT IS NOT SOMETHING TO READ. An interface that only
	// writes must not be counted, or "readable" would mean "has an endpoint".
	outOnly := USBDevice{Interfaces: []USBInterface{
		{Number: 0, Endpoints: []USBEndpoint{{Address: 0x01, Attributes: 0x02}}},
	}}
	if n := len(outOnly.Readable()); n != 0 {
		t.Fatalf("an OUT-only interface was reported as readable")
	}
	// And the camera, whose every IN endpoint is isochronous, has nothing.
	cam := beast(0x05)
	for _, i := range cam.Readable() {
		if i.VideoStreaming() {
			t.Fatalf("an isochronous streaming interface was reported as readable: %s", i)
		}
	}
}
