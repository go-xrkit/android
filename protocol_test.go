// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFramingRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, MsgFrame, []byte("body")); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	typ, body, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if typ != MsgFrame || string(body) != "body" {
		t.Errorf("round trip = 0x%02x %q", typ, body)
	}
}

func TestFramingCarriesAnEmptyBody(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, MsgStop, nil); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	typ, body, err := ReadMessage(&buf)
	if err != nil || typ != MsgStop || len(body) != 0 {
		t.Errorf("empty round trip = 0x%02x %q %v", typ, body, err)
	}
}

func TestWriteMessageReportsAFailedWrite(t *testing.T) {
	if err := WriteMessage(brokenWriter{}, MsgStop, nil); err == nil {
		t.Error("WriteMessage hid a failed write")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("no") }

func TestReadMessageEndsCleanlyBetweenMessages(t *testing.T) {
	if _, _, err := ReadMessage(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("ReadMessage on a closed stream = %v, want io.EOF", err)
	}
}

func TestReadMessageRefusesAnImpossibleLength(t *testing.T) {
	for name, hdr := range map[string][]byte{
		"zero":  {0, 0, 0, 0},
		"huge":  {0xff, 0xff, 0xff, 0xff},
		"above": binary.BigEndian.AppendUint32(nil, MaxPayload+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ReadMessage(bytes.NewReader(hdr))
			if err == nil || !strings.Contains(err.Error(), "out of range") {
				t.Errorf("ReadMessage = %v, want an out-of-range refusal", err)
			}
		})
	}
}

func TestReadMessageRefusesATruncatedMessage(t *testing.T) {
	// A length promising a type byte and a body, with neither present.
	if _, _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 8})); err == nil {
		t.Error("ReadMessage accepted a header with no type byte")
	}
	// A type byte, then a body cut short.
	b := append(binary.BigEndian.AppendUint32(nil, 8), MsgFrame, 1, 2)
	if _, _, err := ReadMessage(bytes.NewReader(b)); err == nil {
		t.Error("ReadMessage accepted a truncated body")
	}
}

func TestConfigRoundTrips(t *testing.T) {
	want := ConfigMessage{Width: 1080, Height: 2400, Stride: 4320,
		Format: FormatRGBA, Slots: 3, SlotSize: 10368000, DisplayID: 0}
	got, err := DecodeConfig(EncodeConfig(want))
	if err != nil || got != want {
		t.Errorf("DecodeConfig(EncodeConfig(%+v)) = %+v, %v", want, got, err)
	}
	if _, err := DecodeConfig(nil); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeConfig(nil) = %v, want ErrShortPayload", err)
	}
}

func TestFrameMsgRoundTrips(t *testing.T) {
	want := FrameMsg{Seq: 574, Slot: 2, Width: 1080, Height: 2400, Stride: 4320,
		AtUnixNano: 1_700_000_000_123_456_789}
	got, err := DecodeFrame(EncodeFrame(want))
	if err != nil || got != want {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	if !got.At().Equal(time.Unix(0, want.AtUnixNano)) {
		t.Errorf("At() = %v", got.At())
	}
	if !(FrameMsg{}).At().IsZero() {
		t.Error("a frame with no timestamp reported the epoch instead of the zero time")
	}
	if _, err := DecodeFrame([]byte{1}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeFrame = %v, want ErrShortPayload", err)
	}
}

func TestStoppedRoundTrips(t *testing.T) {
	for _, want := range []StoppedMessage{
		{Reason: StopUser, Detail: "the cast chip"},
		{Reason: StopSystem},
		{Reason: StopApp, Detail: "closing"},
		{Reason: 99, Detail: "who knows"},
	} {
		got, err := DecodeStopped(EncodeStopped(want))
		if err != nil || got != want {
			t.Errorf("round trip %+v = %+v, %v", want, got, err)
		}
		if got.String() == "" {
			t.Errorf("String() empty for %+v", want)
		}
	}
	if s := (StoppedMessage{Reason: StopUser}).String(); strings.HasSuffix(s, ": ") {
		t.Errorf("String() with no detail left a dangling colon: %q", s)
	}
	if !strings.Contains((StoppedMessage{Reason: StopUser, Detail: "x"}).String(), "user") {
		t.Error("a user stop does not say so")
	}
	if _, err := DecodeStopped(nil); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeStopped(nil) = %v, want ErrShortPayload", err)
	}
	if _, err := DecodeStopped([]byte{StopUser, 0xff}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeStopped with a ruined string = %v, want ErrShortPayload", err)
	}
}

func TestErrorRoundTrips(t *testing.T) {
	want := ErrorMessage{Code: 4, Op: "createVirtualDisplay", Detail: "SecurityException"}
	got, err := DecodeError(EncodeError(want))
	if err != nil || got != want {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	if !strings.Contains(got.Error(), "createVirtualDisplay") || !strings.Contains(got.Error(), "(4)") {
		t.Errorf("Error() = %q", got.Error())
	}
	// No code, and no operation: the message must still read.
	plain := ErrorMessage{Detail: "boom"}.Error()
	if !strings.Contains(plain, "host") || !strings.Contains(plain, "boom") || strings.Contains(plain, "(0)") {
		t.Errorf("Error() with no code = %q", plain)
	}
	if _, err := DecodeError([]byte{0}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeError short = %v", err)
	}
	if _, err := DecodeError([]byte{0, 0, 0, 0, 0xff, 0xff}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeError with a ruined op = %v", err)
	}
	b := append([]byte{0, 0, 0, 0}, 0, 1, 'x', 0xff, 0xff)
	if _, err := DecodeError(b); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeError with a ruined detail = %v", err)
	}
}

func TestDisplaysRoundTrip(t *testing.T) {
	want := []Display{
		{ID: 0, Name: "Built-in Screen", Width: 1080, Height: 2400, DensityDPI: 420,
			RefreshRate: 60.0004, Flags: FlagSecure | FlagSupportsProtectedBuffers},
		{ID: 8, Name: "Overlay #1", Width: 1920, Height: 1080, DensityDPI: 320,
			RefreshRate: 60.0004, Flags: FlagPresentation},
	}
	got, err := DecodeDisplays(EncodeDisplays(want))
	if err != nil {
		t.Fatalf("DecodeDisplays: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d displays, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name ||
			got[i].Width != want[i].Width || got[i].Height != want[i].Height ||
			got[i].DensityDPI != want[i].DensityDPI || got[i].Flags != want[i].Flags {
			t.Errorf("display %d = %+v, want %+v", i, got[i], want[i])
		}
		// The rate crosses as milli-hertz, so it comes back rounded, not equal.
		if d := got[i].RefreshRate - want[i].RefreshRate; d > 0.001 || d < -0.001 {
			t.Errorf("display %d refresh %v, want %v", i, got[i].RefreshRate, want[i].RefreshRate)
		}
	}
	if ds, err := DecodeDisplays(EncodeDisplays(nil)); err != nil || len(ds) != 0 {
		t.Errorf("empty list round trip = %v, %v", ds, err)
	}
}

func TestDecodeDisplaysRefusesRubbish(t *testing.T) {
	cases := map[string][]byte{
		"no count":       {0, 0},
		"negative count": {0xff, 0xff, 0xff, 0xff},
		"count too big":  {0, 0, 0, 9},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDisplays(b); err == nil {
				t.Error("DecodeDisplays accepted rubbish")
			}
		})
	}
	// One display whose entry is long enough to pass the cheap bound but is
	// cut short inside its name, and one cut short after it.
	long := make([]byte, 30)
	binary.BigEndian.PutUint32(long, 1)
	binary.BigEndian.PutUint16(long[8:], 0xff)
	if _, err := DecodeDisplays(long); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeDisplays with a ruined name = %v", err)
	}
	afterName := make([]byte, 30)
	binary.BigEndian.PutUint32(afterName, 1)
	binary.BigEndian.PutUint16(afterName[8:], 20) // a 20-byte name eats the rest
	if _, err := DecodeDisplays(afterName); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeDisplays truncated after the name = %v", err)
	}
	// A count of one with the id itself missing.
	two := []byte{0, 0, 0, 1, 0, 0}
	if _, err := DecodeDisplays(two); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeDisplays with no id = %v", err)
	}
}

// TestDecodeDisplaysRefusesATruncatedEntryMidList exercises the per-entry
// length check, which the cheap up-front bound cannot reach on its own.
func TestDecodeDisplaysRefusesATruncatedEntryMidList(t *testing.T) {
	full := EncodeDisplays([]Display{
		{ID: 1, Name: "aaaaaaaaaaaaaaaa"},
		{ID: 2, Name: "b"},
	})
	// Keep the count, keep the first entry, keep only two bytes of the second.
	cut := full[:len(full)-25]
	if _, err := DecodeDisplays(cut); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeDisplays on a mid-list truncation = %v, want ErrShortPayload", err)
	}
}

// TestDecodeDisplaysRefusesAnEntryStarvedByItsPredecessor reaches the per-entry
// bound, which the cheap up-front one cannot: a long name in an early entry
// eats the budget the later ones were counted against.
func TestDecodeDisplaysRefusesAnEntryStarvedByItsPredecessor(t *testing.T) {
	b := binary.BigEndian.AppendUint32(nil, 2) // two displays promised
	b = binary.BigEndian.AppendUint32(b, 1)    // id
	b = binary.BigEndian.AppendUint16(b, 30)   // a 30-byte name
	b = append(b, make([]byte, 30)...)
	b = append(b, make([]byte, 20)...) // its four fields and its flags
	b = append(b, 0, 0)                // and two bytes where the second entry should be
	if _, err := DecodeDisplays(b); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeDisplays = %v, want ErrShortPayload", err)
	}
}

func TestStartRoundTrips(t *testing.T) {
	want := StartMessage{DisplayID: 0, Width: 1080, Height: 2400, MilliFPS: 60000, Slots: 3}
	got, err := DecodeStart(EncodeStart(want))
	if err != nil || got != want {
		t.Errorf("round trip = %+v, %v", got, err)
	}
	if _, err := DecodeStart([]byte{1}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeStart = %v, want ErrShortPayload", err)
	}
}

func TestBufferRoundTrips(t *testing.T) {
	slots, size, err := DecodeBuffer(EncodeBuffer(3, 10368000))
	if err != nil || slots != 3 || size != 10368000 {
		t.Errorf("round trip = %d, %d, %v", slots, size, err)
	}
	if _, _, err := DecodeBuffer([]byte{1}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeBuffer = %v, want ErrShortPayload", err)
	}
}

func TestConsentRoundTrips(t *testing.T) {
	for _, want := range []bool{true, false} {
		got, err := DecodeConsent(EncodeConsent(want))
		if err != nil || got != want {
			t.Errorf("round trip %v = %v, %v", want, got, err)
		}
	}
	if _, err := DecodeConsent(nil); !errors.Is(err, ErrShortPayload) {
		t.Errorf("DecodeConsent(nil) = %v, want ErrShortPayload", err)
	}
}

// TestALongStringIsTruncatedNotMisencoded: a display name is cosmetic, but a
// length field that overflowed would desynchronise the whole stream.
func TestALongStringIsTruncatedNotMisencoded(t *testing.T) {
	name := strings.Repeat("x", 0x10005)
	ds, err := DecodeDisplays(EncodeDisplays([]Display{{ID: 1, Name: name}}))
	if err != nil {
		t.Fatalf("DecodeDisplays: %v", err)
	}
	if len(ds[0].Name) != 0xffff {
		t.Errorf("name came back %d bytes, want it truncated to 65535", len(ds[0].Name))
	}
}

func TestTakeStringRefusesAMissingLength(t *testing.T) {
	if _, _, err := takeString([]byte{1}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("takeString = %v, want ErrShortPayload", err)
	}
}

func TestDeriveSocket(t *testing.T) {
	for name, tc := range map[string]struct {
		env   string
		envOK bool
		home  string
		want  string
	}{
		"an explicit name wins":        {"gw-1234", true, "/data/user/0/org.example.app/files", "gw-1234"},
		"derived from a modern home":   {"", false, "/data/user/0/org.example.app/files", "org.example.app.xr"},
		"derived from a legacy home":   {"", false, "/data/data/org.example.app/files", "org.example.app.xr"},
		"another user":                 {"", false, "/data/user/10/org.example.app/files", "org.example.app.xr"},
		"an empty variable is no name": {"", true, "/data/user/0/org.example.app/files", "org.example.app.xr"},
		"no home at all":               {"", false, "", ""},
		"a home that is not an app":    {"", false, "/root", ""},
		"a home with no package":       {"", false, "/data/user/0", ""},
		"a user number and nothing":    {"", false, "/data/user/0/", ""},
		"not a package name":           {"", false, "/data/user/0/notapackage/files", ""},
		"legacy with no package":       {"", false, "/data/data/", ""},
		"no slash after the user":      {"", false, "/data/user/0000", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := DeriveSocket(tc.env, tc.envOK, tc.home); got != tc.want {
				t.Errorf("DeriveSocket(%q, %v, %q) = %q, want %q",
					tc.env, tc.envOK, tc.home, got, tc.want)
			}
		})
	}
}

func TestMessageIdsDoNotCollide(t *testing.T) {
	// Host→app below 0x80, app→host at or above it, so a misrouted message is
	// a decode error rather than a plausible other message.
	hostToApp := map[string]uint8{
		"MsgDisplays": MsgDisplays, "MsgConfig": MsgConfig, "MsgFrame": MsgFrame,
		"MsgStopped": MsgStopped, "MsgConsent": MsgConsent, "MsgError": MsgError,
		"MsgBuffer": MsgBuffer,
	}
	appToHost := map[string]uint8{
		"MsgListDisplays": MsgListDisplays, "MsgConsentRequest": MsgConsentRequest,
		"MsgStart": MsgStart, "MsgStop": MsgStop, "MsgBye": MsgBye,
	}
	seen := map[uint8]string{}
	for _, m := range []struct {
		ids  map[string]uint8
		high bool
	}{{hostToApp, false}, {appToHost, true}} {
		for name, id := range m.ids {
			if other, dup := seen[id]; dup {
				t.Errorf("0x%02x is both %s and %s", id, name, other)
			}
			seen[id] = name
			if (id >= 0x80) != m.high {
				t.Errorf("%s = 0x%02x is on the wrong side of 0x80", name, id)
			}
		}
	}
}

func TestScreenMessagesSurviveARoundTrip(t *testing.T) {
	open := OpenScreenMessage{DisplayID: 12, Width: 1920, Height: 1080, Slots: 3}
	gotOpen, err := DecodeOpenScreen(EncodeOpenScreen(open))
	if err != nil {
		t.Fatalf("DecodeOpenScreen: %v", err)
	}
	if gotOpen != open {
		t.Fatalf("open-screen round-tripped to %+v, want %+v", gotOpen, open)
	}

	pres := PresentMessage{Seq: 1 << 40, Slot: 2, Width: 1920, Height: 1080, Stride: 7680}
	gotPres, err := DecodePresent(EncodePresent(pres))
	if err != nil {
		t.Fatalf("DecodePresent: %v", err)
	}
	if gotPres != pres {
		t.Fatalf("present round-tripped to %+v, want %+v", gotPres, pres)
	}

	ack := PresentedMessage{Seq: 1 << 40, Slot: 2}
	gotAck, err := DecodePresented(EncodePresented(ack))
	if err != nil {
		t.Fatalf("DecodePresented: %v", err)
	}
	if gotAck != ack {
		t.Fatalf("presented round-tripped to %+v, want %+v", gotAck, ack)
	}
}

// A body one byte short of every screen message. The host is a separate
// process: a truncated body is a desynchronised stream, and reading past it
// would decode the NEXT message's bytes as this one's geometry.
func TestScreenMessagesRefuseABodyThatIsTooShort(t *testing.T) {
	for _, c := range []struct {
		name string
		body []byte
		call func([]byte) error
	}{
		{"open-screen", make([]byte, 15), func(b []byte) error { _, err := DecodeOpenScreen(b); return err }},
		{"present", make([]byte, 23), func(b []byte) error { _, err := DecodePresent(b); return err }},
		{"presented", make([]byte, 11), func(b []byte) error { _, err := DecodePresented(b); return err }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(c.body); !errors.Is(err, ErrShortPayload) {
				t.Fatalf("decoding %d bytes reported %v, want ErrShortPayload", len(c.body), err)
			}
		})
	}
}

// ⛔ HOST→APP BELOW 0x80, APP→HOST AT OR ABOVE IT. A new message on the wrong
// side of that line would be a plausible other message rather than a decode
// error, which is the one failure this numbering exists to prevent.
func TestTheScreenMessagesAreNumberedOnTheRightSide(t *testing.T) {
	for name, typ := range map[string]uint8{"MsgPresented": MsgPresented} {
		if typ >= 0x80 {
			t.Fatalf("%s is %#02x, which is the app→host range", name, typ)
		}
	}
	for name, typ := range map[string]uint8{"MsgOpenScreen": MsgOpenScreen, "MsgPresent": MsgPresent} {
		if typ < 0x80 {
			t.Fatalf("%s is %#02x, which is the host→app range", name, typ)
		}
	}
}

func TestCensusMessagesSurviveARoundTrip(t *testing.T) {
	cams := []Camera{
		{ID: "0", Facing: FacingBack, Width: 4000, Height: 3000},
		{ID: "ext-usb-7", Facing: FacingExternal, Width: 1920, Height: 1080},
	}
	gotCams, err := DecodeCameras(EncodeCameras(cams))
	if err != nil {
		t.Fatalf("DecodeCameras: %v", err)
	}
	if len(gotCams) != len(cams) {
		t.Fatalf("got %d cameras, want %d", len(gotCams), len(cams))
	}
	for i := range cams {
		if gotCams[i] != cams[i] {
			t.Fatalf("camera %d round-tripped to %+v, want %+v", i, gotCams[i], cams[i])
		}
	}

	devs := []USBDevice{{
		Name: "/dev/bus/usb/001/002", VendorID: VitureVendorID, ProductID: 0x1011,
		Manufacturer: "VITURE", Product: "VITURE Beast", Class: USBClassPerInterface,
		Interfaces: []USBInterface{
			{Number: 1, Alternate: 0, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming},
			{Number: 1, Alternate: 1, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
				Endpoints: []USBEndpoint{{Address: 0x81, Attributes: 0x05, MaxPacketSize: 3072, Interval: 1}}},
		},
	}, {Name: "/dev/bus/usb/001/003"}}
	gotDevs, err := DecodeUSBDevices(EncodeUSBDevices(devs))
	if err != nil {
		t.Fatalf("DecodeUSBDevices: %v", err)
	}
	if len(gotDevs) != 2 || len(gotDevs[0].Interfaces) != 2 ||
		len(gotDevs[0].Interfaces[1].Endpoints) != 1 {
		t.Fatalf("the device tree did not survive: %+v", gotDevs)
	}
	// ⛔ THE ENDPOINT IS THE ANSWER THE CENSUS EXISTS FOR, so it is compared
	// field by field rather than counted: a transport that dropped bmAttributes
	// would make every camera look readable.
	if got, want := gotDevs[0].Interfaces[1].Endpoints[0], devs[0].Interfaces[1].Endpoints[0]; got != want {
		t.Fatalf("the endpoint round-tripped to %+v, want %+v", got, want)
	}
	if !gotDevs[0].Viture() || gotDevs[0].BulkVideo() {
		t.Fatalf("the decoded device is %s", gotDevs[0])
	}
	// A device with nothing in it must survive too: an empty interface list is
	// what a hub or a charger looks like.
	if len(gotDevs[1].Interfaces) != 0 || gotDevs[1].Name != "/dev/bus/usb/001/003" {
		t.Fatalf("the empty device round-tripped to %+v", gotDevs[1])
	}
}

// Every way a census message can be truncated or impossible. The host is a
// separate process, and a count it announced must never be believed far enough
// to allocate for.
func TestCensusMessagesRefuseWhatCannotBeDecoded(t *testing.T) {
	// A count of two billion with no bytes behind it. Believing it would turn
	// one message into an out-of-memory.
	huge := appendInt32(nil, 1<<30)
	for _, c := range []struct {
		name string
		body []byte
		call func([]byte) error
		is   error
	}{
		{"cameras, no count", make([]byte, 3),
			func(b []byte) error { _, err := DecodeCameras(b); return err }, ErrShortPayload},
		{"cameras, negative count", appendInt32(nil, -1),
			func(b []byte) error { _, err := DecodeCameras(b); return err }, ErrBadPayload},
		{"cameras, a count nothing could fill", huge,
			func(b []byte) error { _, err := DecodeCameras(b); return err }, ErrShortPayload},
		{"cameras, truncated after the id", append(appendInt32(nil, 1), 0, 1, 'x', 0, 0),
			func(b []byte) error { _, err := DecodeCameras(b); return err }, ErrShortPayload},
		{"usb, no count", make([]byte, 3),
			func(b []byte) error { _, err := DecodeUSBDevices(b); return err }, ErrShortPayload},
		{"usb, negative count", appendInt32(nil, -1),
			func(b []byte) error { _, err := DecodeUSBDevices(b); return err }, ErrBadPayload},
		{"usb, truncated after the name", append(appendInt32(nil, 1), 0, 0, 1, 2),
			func(b []byte) error { _, err := DecodeUSBDevices(b); return err }, ErrShortPayload},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(c.body)
			if !errors.Is(err, c.is) {
				t.Fatalf("decoding %d bytes reported %v, want %v", len(c.body), err, c.is)
			}
		})
	}
}

// A device whose counts are impossible, built field by field so each refusal is
// reached deliberately rather than by chance.
func TestAUSBDeviceWithImpossibleCountsIsRefused(t *testing.T) {
	head := func() []byte {
		b := appendString(nil, "/dev/bus/usb/001/002")
		b = appendInt32(b, 0x35ca)
		b = appendInt32(b, 1)
		b = appendString(b, "")
		return appendString(b, "")
	}
	negIfaces := append(appendInt32(nil, 1), head()...)
	negIfaces = appendInt32(negIfaces, 0)
	negIfaces = appendInt32(negIfaces, 0)
	negIfaces = appendInt32(negIfaces, 0)
	negIfaces = appendInt32(negIfaces, -1)
	if _, err := DecodeUSBDevices(negIfaces); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("a device with -1 interfaces reported %v", err)
	}

	truncIface := append(appendInt32(nil, 1), head()...)
	for _, v := range []int{0, 0, 0, 1} {
		truncIface = appendInt32(truncIface, v)
	}
	if _, err := DecodeUSBDevices(truncIface); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a device promising an interface that is not there reported %v", err)
	}

	negEps := append(appendInt32(nil, 1), head()...)
	for _, v := range []int{0, 0, 0, 1 /* iface */, 0, 0, 0, 0, 0, -1} {
		negEps = appendInt32(negEps, v)
	}
	if _, err := DecodeUSBDevices(negEps); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("an interface with -1 endpoints reported %v", err)
	}

	truncEp := append(appendInt32(nil, 1), head()...)
	for _, v := range []int{0, 0, 0, 1 /* iface */, 0, 0, 0, 0, 0, 1} {
		truncEp = appendInt32(truncEp, v)
	}
	if _, err := DecodeUSBDevices(truncEp); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("an interface promising an endpoint that is not there reported %v", err)
	}

	// And a device truncated between its name and its strings.
	short := append(appendInt32(nil, 1), appendString(nil, "x")...)
	short = appendInt32(short, 1)
	short = appendInt32(short, 1)
	short = append(short, 0) // half a string length
	if _, err := DecodeUSBDevices(short); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a device truncated mid-string reported %v", err)
	}
}

// A length-prefixed string whose bytes are not all there. It is its own test
// because the obvious truncation — stopping after a COMPLETE string — reaches a
// different branch, and the two were confused once already: a body that ends
// mid-string must be refused before the length is used to slice.
func TestACensusStringThatIsNotAllThereIsRefused(t *testing.T) {
	// A string claiming five bytes with one behind it.
	cut := []byte{0x00, 0x05, 'a'}

	if _, err := DecodeCameras(append(appendInt32(nil, 1), cut...)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a camera id cut short reported %v", err)
	}
	if _, err := DecodeUSBDevices(append(appendInt32(nil, 1), cut...)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a usb device name cut short reported %v", err)
	}

	// Past the name and the ids, with the PRODUCT string cut short.
	prod := append(appendInt32(nil, 1), appendString(nil, "/dev/bus/usb/001/002")...)
	prod = appendInt32(prod, 0x35ca)
	prod = appendInt32(prod, 1)
	prod = appendString(prod, "VITURE")
	prod = append(prod, cut...)
	if _, err := DecodeUSBDevices(prod); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a usb product string cut short reported %v", err)
	}

	// Both strings complete, and then nothing where the class triple and the
	// interface count should be.
	after := append(appendInt32(nil, 1), appendString(nil, "/dev/bus/usb/001/002")...)
	after = appendInt32(after, 0x35ca)
	after = appendInt32(after, 1)
	after = appendString(after, "")
	after = appendString(after, "")
	after = append(after, 1, 2, 3)
	if _, err := DecodeUSBDevices(after); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a usb device truncated after its strings reported %v", err)
	}
}

// ⛔ THE CHEAP GUARD HIDES THE CAREFUL ONES. DecodeCameras refuses a count that
// could not possibly fit before it allocates, and that guard swallows every
// short body an obvious test writes — so the per-camera checks behind it went
// unexercised while looking tested. These bodies are long enough to get past
// it and still wrong.
func TestACameraListLongEnoughToPassTheCheapGuardAndStillWrong(t *testing.T) {
	// 15 bytes for one camera, so the count guard lets it through — and the id
	// claims 255 bytes with 13 behind it.
	cut := append(appendInt32(nil, 1), 0x00, 0xff)
	cut = append(cut, make([]byte, 13)...)
	if _, err := DecodeCameras(cut); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a camera id claiming more than the body holds reported %v", err)
	}

	// A complete ten-character id, and then three bytes where twelve of facing
	// and size should be.
	short := append(appendInt32(nil, 1), appendString(nil, "0123456789")...)
	short = append(short, 1, 2, 3)
	if _, err := DecodeCameras(short); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("a camera truncated after its id reported %v", err)
	}
}
