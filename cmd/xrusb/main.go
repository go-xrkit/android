// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrusb answers ONE question: will Android let an ordinary application
// submit an isochronous USB transfer?
//
// # Why everything depends on it
//
// A head is followed from the headset's camera — the VITURE Beast publishes no
// orientation, and go-xrkit/xrkit/headflow recovers the yaw from its pictures.
// cmd/xrcam established that on this phone both of Android's own routes to that
// camera are shut: camera2 enumerates no external device, and every UVC
// streaming endpoint is isochronous, which UsbDeviceConnection cannot submit at
// all.
//
// usbfs is what is left — the kernel's own interface, reached by ioctl on the
// descriptor UsbDeviceConnection hands out. It is plain syscalls, so a CGO-free
// Go process can do it on paper. Whether SELinux permits it is undocumented.
//
// ⛔ AND A UVC IMPLEMENTATION IS WORTHLESS IF THE ANSWER IS NO. Descriptor
// parsing, probe/commit negotiation, frame assembly, MJPEG decoding — days of
// it, all resting on one ioctl nobody has tried. So this tries the ioctl, in
// four steps, and says which one the kernel stopped at.
//
//	APP=./cmd/xrusb host/build.sh && adb install -r host/out/xrhost.apk
//	adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity \
//	    --ez capture false --es args '-wait 5m'
//	adb shell run-as org.goxrkit.androidhost cat files/usb.txt
//
// ⚠ IT PUTS A DIALOG IN FRONT OF SOMEBODY. Opening a USB device asks the user,
// naming the device and this application. The transcript says so, and a refusal
// is reported as a decision rather than as a failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-xrkit/android"
)

func main() { os.Exit(run(context.Background(), os.Args[1:])) }

func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("xrusb", flag.ContinueOnError)
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for a UVC camera to appear")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var out strings.Builder
	say := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Println(line)
		out.WriteString(line + "\n")
	}
	defer func() { save(out.String(), say) }()

	if !android.WallAvailable() {
		say("FAIL no wall host: this must run inside the APK")
		return 1
	}
	dev, ok := awaitCamera(ctx, *wait, say)
	if !ok {
		return 1
	}
	say("TARGET %s", dev)

	say("")
	say("⚠ A PERMISSION DIALOG IS ABOUT TO APPEAR on the phone, naming this " +
		"application and that device. Nothing below happens until somebody taps it.")
	h, err := android.OpenUSB(ctx, dev)
	if err != nil {
		say("STOPPED at open: %v", err)
		say("RESULT the descriptor was never obtained, so the question is unanswered — " +
			"this says nothing about whether the kernel would have allowed the transfer")
		return 1
	}
	defer func() {
		if err := h.Close(); err != nil {
			say("⚠ closing the handle: %v", err)
		}
	}()
	say("STEP 1 open                  OK — the host lent the kernel's descriptor")

	// ⭐ A PLAIN read(2), not an ioctl: it needs nothing beyond the open, so a
	// failure here is the descriptor being useless and every later failure is
	// narrowed to the ioctl that produced it.
	desc, err := h.Descriptors()
	if err != nil {
		say("STOPPED at descriptors: %v", err)
		return 1
	}
	v, p, cl, sub, proto, err := desc.Device()
	if err != nil {
		say("STOPPED decoding the device descriptor: %v", err)
		return 1
	}
	say("STEP 2 read descriptors      OK — %d bytes, %04x:%04x class %#02x/%#02x/%#02x",
		len(desc), v, p, cl, sub, proto)

	ifaces, err := desc.Interfaces()
	if err != nil {
		say("STOPPED walking the descriptors: %v", err)
		return 1
	}
	iface, ep, found := streamingEndpoint(ifaces)
	if !found {
		say("STOPPED: none of the %d interfaces has an isochronous IN endpoint on a UVC "+
			"streaming interface", len(ifaces))
		for _, i := range ifaces {
			say("  %s", i)
		}
		return 1
	}
	say("       streaming on interface %d alternate %d, %s", iface.Number, iface.Alternate, ep)

	if err := h.ClaimInterface(iface.Number); err != nil {
		say("STOPPED at claim: %v", err)
		say("RESULT the kernel would not give up interface %d. Its own uvcvideo driver "+
			"usually holds a camera, and an application that cannot take it cannot "+
			"transfer on it either.", iface.Number)
		return 1
	}
	say("STEP 3 claim interface %d     OK — taken from whatever driver held it", iface.Number)

	if err := h.SetAltSetting(iface.Number, iface.Alternate); err != nil {
		say("STOPPED at set-alternate: %v", err)
		say("RESULT the bandwidth could not be reserved; an isochronous endpoint has none " +
			"until its alternate is selected, so the transfer below could not work anyway")
		return 1
	}
	say("STEP 4 select alternate %d    OK — bandwidth reserved", iface.Alternate)

	// ⛔ THE QUESTION. Everything above is ordinary; this is the part Android's
	// Java API cannot do at all and that nothing documents for usbfs.
	say("")
	if err := h.SubmitISO(ep); err != nil {
		say("ANSWER NO — the kernel refused the isochronous request: %v", err)
		say("RESULT this phone will not let an ordinary application read that camera by " +
			"any route. camera2 does not offer it, UsbDeviceConnection cannot submit " +
			"isochronous transfers, and usbfs refuses them here too. Head tracking from " +
			"the headset's camera is NOT POSSIBLE on this device without a privileged " +
			"component.")
		return 1
	}
	say("ANSWER YES — the kernel ACCEPTED an isochronous request on endpoint %#02x",
		ep.Address)
	say("RESULT usbfs is a usable route. What remains is a UVC implementation: the "+
		"probe/commit negotiation, frame assembly from the packet descriptors, and "+
		"decoding whatever format %d:%d advertises. None of it is written.",
		iface.Number, iface.Alternate)
	say("⚠ ACCEPTED IS NOT STREAMING. This submitted one request and cancelled it; it " +
		"says the kernel takes the call, not that pixels arrive.")
	return 0
}

// streamingEndpoint picks the UVC streaming interface's alternate with the
// LARGEST isochronous IN endpoint.
//
// ⭐ THE LARGEST, because the alternates are a bandwidth ladder and the biggest
// is the one a real stream would want — and because it is the one most likely to
// be refused for bandwidth, which is a different refusal from being refused for
// permission and worth telling apart.
func streamingEndpoint(ifaces []android.USBInterface) (android.USBInterface, android.USBEndpoint, bool) {
	var bi android.USBInterface
	var be android.USBEndpoint
	found := false
	for _, i := range ifaces {
		if !i.VideoStreaming() {
			continue
		}
		for _, e := range i.Endpoints {
			if e.In() && e.Type() == android.USBTransferIsochronous &&
				e.MaxPacketSize > be.MaxPacketSize {
				bi, be, found = i, e, true
			}
		}
	}
	return bi, be, found
}

// awaitCamera waits for a UVC device to be attached, taking a witness first.
//
// ⚠ THE CAMERA IS A SEPARATE DEVICE FROM THE GLASSES. The Beast arrives as
// three: a Sonix UVC camera, a microphone, and the headset itself. A probe that
// waited for "a VITURE device" would open the wrong one.
func awaitCamera(ctx context.Context, budget time.Duration, say func(string, ...any)) (android.USBDevice, bool) {
	known := map[string]bool{}
	first := true
	deadline := time.Now().Add(budget)
	for {
		devs, err := android.USBDevices(ctx)
		if err != nil {
			say("FAIL USBDevices: %v", err)
			return android.USBDevice{}, false
		}
		if first {
			say("BEFORE %d USB device(s) attached:", len(devs))
			for _, d := range devs {
				known[d.Name] = true
				say("  %s %04x:%04x %q", d.Name, d.VendorID, d.ProductID, d.Product)
			}
			say("WAITING up to %s for a UVC camera", budget)
			first = false
		}
		for _, d := range devs {
			if len(d.VideoStreaming()) > 0 {
				return d, true
			}
		}
		if !time.Now().Before(deadline) {
			say("FAIL no UVC camera appeared in %s", budget)
			return android.USBDevice{}, false
		}
		select {
		case <-ctx.Done():
			say("FAIL cancelled while waiting")
			return android.USBDevice{}, false
		case <-time.After(2 * time.Second):
		}
	}
}

func save(text string, say func(string, ...any)) {
	path, err := android.SaveTranscript("usb.txt", text)
	if err != nil {
		say("⚠ the transcript could not be saved: %v", err)
		return
	}
	fmt.Printf("ARTIFACT %s\n", path)
}
