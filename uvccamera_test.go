// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import "testing"

// ⛔⛔ A ZERO-BANDWIDTH ALTERNATE IS A STREAMING INTERFACE WITH NO ENDPOINT, and
// that is why this is two loops rather than one condition.
//
// Alternate 0 of a UVC streaming interface carries no endpoint at all — that is
// what zero bandwidth means, and it is where this package deliberately parks
// the interface while it negotiates. A camera whose descriptor lists only that
// alternate therefore has a streaming interface with an empty endpoint list,
// and a single `VideoStreaming() && len(Endpoints) > 0` would report it as
// having no streaming interface: the open would fail with "no UVC streaming
// interface on …" against a camera that was plainly there.
//
// ⭐ The function was pure and unreachable inside cmd/xrhead until the open
// moved into the package. This is the test that could not be written before.
func TestTheStreamingInterfaceIsFoundWithAndWithoutEndpoints(t *testing.T) {
	t.Parallel()

	// A plain audio or HID interface, to make sure the class is what decides.
	other := USBInterface{Number: 0}
	withEP := USBInterface{
		Number:    1,
		Class:     USBClassVideo,
		Subclass:  USBSubclassVideoStreaming,
		Endpoints: []USBEndpoint{{Address: 0x81}},
	}
	bare := USBInterface{
		Number:   2,
		Class:    USBClassVideo,
		Subclass: USBSubclassVideoStreaming,
	}

	for _, c := range []struct {
		name   string
		ifaces []USBInterface
		want   int
		found  bool
	}{
		{"one with an endpoint", []USBInterface{other, withEP}, 1, true},
		{
			// ⛔ The case the second loop exists for.
			name:   "only a zero-bandwidth alternate, so no endpoint",
			ifaces: []USBInterface{other, bare},
			want:   2, found: true,
		},
		{
			// ⛔ AND THE ONE WITH AN ENDPOINT WINS when both are listed: it is
			// the one that can carry a frame, and picking the other would park
			// the stream on an interface with nowhere to read from.
			name:   "both listed, the one with an endpoint wins",
			ifaces: []USBInterface{bare, withEP},
			want:   1, found: true,
		},
		{"nothing streaming at all", []USBInterface{other}, 0, false},
		{"no interfaces at all", nil, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got, ok := streamingInterface(c.ifaces)
			if ok != c.found {
				t.Fatalf("streamingInterface found=%v, want %v (got interface %d)", ok, c.found, got)
			}
			if ok && got != c.want {
				t.Errorf("streamingInterface = %d, want %d", got, c.want)
			}
		})
	}
}
