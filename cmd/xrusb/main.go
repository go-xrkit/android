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
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/go-xrkit/android"
)

func main() { os.Exit(run(context.Background(), os.Args[1:])) }

func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("xrusb", flag.ContinueOnError)
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for a UVC camera to appear")
	// ⭐ A FLAG BECAUSE THE QUESTION IS A SWEEP, not a setting. The byte budget
	// of the 640x480 run closes on the camera UNDER-FILLING each interval —
	// 832 bytes of the 2400 it committed to, 34.7% — which is 10.5 frames a
	// second of data and is exactly what came out. 25 fps at 640x480 needs 1920
	// bytes an interval; 320x240 needs 480. So a smaller frame should reach the
	// full rate, and that is a prediction rather than a preference.
	width := fs.Int("width", 0, "force a frame width (0 picks the fastest above "+
		"MinTrackingWidth, which is what a real caller gets)")
	// ⭐ MJPEG IS NOT THE DEFAULT, AND THAT IS DELIBERATE. The camera's table
	// says MJPEG does 30 fps at every size against YUY2's 25 at 640x480 and
	// below, and the byte budget says why — but what MJPEG costs is a DECODE,
	// and the processor that has to fit it inside a frame interval is this
	// phone's rather than a workstation's. Until that is measured here, changing
	// the default would be acting on a prediction. The stream reports its own
	// decoding time, so one run settles it.
	format := fs.String("format", "", "force a format: yuy2 or mjpeg (empty asks "+
		"PickFormat, which prefers the one needing no decoder)")
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
	reportV4L2(say)
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

	// ⛔⛔ ALTERNATE 0 HERE, AND THE OPERATING ONE ONLY AFTER THE COMMIT. This
	// probe selected the streaming alternate FIRST and then negotiated, and the
	// camera sent nothing at all for fifteen seconds while the bus happily
	// delivered empty packets:
	//
	//	STEP 8 stream   0 frame(s) in 15s
	//	    119464 packets (119464 empty), 0 bytes
	//
	// Measured on a Pixel 11 Pro Fold, 2026-10-11. The plumbing was never the
	// problem — 119464 packets in 15s is one request a millisecond, exactly the
	// isochronous schedule. UVC takes the move AWAY from the zero-bandwidth
	// alternate, after a commit, as the signal to start sending; arriving there
	// before the commit means that signal never happens. It is the order the
	// kernel's own uvcvideo uses, and the order this package now documents.
	if err := h.SetAltSetting(iface.Number, android.ZeroBandwidthAlternate); err != nil {
		say("STOPPED at set-alternate 0: %v", err)
		say("RESULT the interface could not be parked at zero bandwidth, which is where " +
			"UVC requires it to sit while it is negotiated")
		return 1
	}
	say("STEP 4 park at alternate %d   OK — zero bandwidth, ready to negotiate",
		android.ZeroBandwidthAlternate)

	// ⭐ ASK THE CAMERA ITSELF, WITH THE INTERFACE PARKED. Everything below is
	// control transfers: the device agreeing to produce bytes, which is a
	// separate question from the kernel agreeing to carry them.
	say("")
	formats, ferr := desc.UVCFormats(iface.Number)
	if ferr != nil {
		say("STEP 5 formats               FAILED: %v", ferr)
		return 1
	}
	say("STEP 5 read formats          OK — %d format(s) on interface %d",
		len(formats), iface.Number)
	for _, f := range formats {
		say("    %s", f)
	}
	want, size, perr := android.PickFormat(formats)
	if perr != nil {
		say("STEP 6 pick                  FAILED: %v", perr)
		return 1
	}
	if *width > 0 || *format != "" {
		kind := want.Format
		switch strings.ToLower(*format) {
		case "":
		case "yuy2":
			kind = android.FormatYUY2
		case "mjpeg":
			kind = android.FormatMJPEG
		default:
			say("STEP 6 pick                  FAILED: %q is not a format this probe "+
				"knows; it takes yuy2 or mjpeg", *format)
			return 1
		}
		at := *width
		if at == 0 {
			at = size.Width
		}
		w, z, ok := frameAt(formats, kind, at)
		if !ok {
			say("STEP 6 pick                  FAILED: %s offers no %d-pixel-wide frame; "+
				"the table above is what the camera has", kind, at)
			return 1
		}
		want, size = w, z
		say("STEP 6 pick                  %s %s (forced: -format %s -width %d)",
			want.Format, size, kind, at)
	} else {
		say("STEP 6 pick                  %s %s", want.Format, size)
	}

	neg, nerr := h.Negotiate(iface.Number, want, size)
	if nerr != nil {
		say("STEP 7 negotiate             FAILED: %v", nerr)
		say("RESULT the kernel carries the transfer and the CAMERA would not settle on a " +
			"format. That is a device conversation rather than a platform limit, and the " +
			"error above says which step of probe/commit it refused.")
		return 1
	}
	say("STEP 7 negotiate             OK — %s", neg)
	if neg.Changed() {
		say("⚠ THE CAMERA CHANGED THE TERMS. It is entitled to, and what it answered is " +
			"what will arrive — not what was asked for.")
	}

	// ⛔⛔ NOW the operating alternate, chosen from what was COMMITTED. The
	// smallest that carries the payload: an alternate holds bus bandwidth for as
	// long as it is selected, and this camera's largest offers 3072 bytes an
	// interval to carry 2400.
	ifaces, ierr := desc.Interfaces()
	if ierr != nil {
		say("STEP 7b alternate            FAILED reading the interfaces again: %v", ierr)
		return 1
	}
	alt, aep, ok := android.PickAlternate(ifaces, iface.Number, neg.MaxPayload)
	if !ok {
		say("STEP 7b alternate            FAILED: no alternate of interface %d carries "+
			"%d bytes an interval", iface.Number, neg.MaxPayload)
		return 1
	}
	if err := h.SetAltSetting(alt.Number, alt.Alternate); err != nil {
		say("STEP 7b alternate            FAILED: %v", err)
		say("RESULT the bandwidth could not be reserved; an isochronous endpoint has none " +
			"until its alternate is selected, so nothing could arrive anyway")
		return 1
	}
	say("STEP 7b alternate %d          OK — %d bytes an interval for a %d-byte payload",
		alt.Alternate, aep.ISOPacketSize(), neg.MaxPayload)
	ep = aep

	// ⛔ THE QUESTION. Everything above is ordinary; this is the part Android's
	// Java API cannot do at all and that nothing documents for usbfs.
	say("")
	if err := h.SubmitISO(ep); err != nil {
		// ⛔⛔ A MALFORMED REQUEST IS NOT A REFUSAL, AND THIS SAID IT WAS. The
		// first run printed "the kernel refused" on EMSGSIZE, which came from
		// passing wMaxPacketSize where the bytes-per-interval belonged — our
		// own bug, announced as the platform's verdict. The errno is the only
		// thing that tells the two apart, so it decides what this prints.
		switch {
		case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
			say("ANSWER NO — the kernel REFUSED the isochronous request: %v", err)
			say("RESULT an ordinary application may not read this camera by any route: " +
				"camera2 does not offer it, UsbDeviceConnection cannot submit " +
				"isochronous transfers, and usbfs refuses them here too.")
		default:
			say("ANSWER UNKNOWN — not accepted, and NOT because it was refused: %v", err)
			say("RESULT that is a malformed request rather than a verdict. EMSGSIZE, "+
				"EINVAL and ENODEV all come from WHAT WAS ASKED rather than from "+
				"whether it was allowed. The endpoint carries %d bytes an interval "+
				"(wMaxPacketSize %d decoded); fix the request and ask again.",
				ep.ISOPacketSize(), ep.MaxPacketSize)
		}
		return 1
	}
	say("ANSWER YES — the kernel ACCEPTED an isochronous request on endpoint %#02x",
		ep.Address)
	{
	}

	// ⛔ AND NOW THE ONLY QUESTION THAT WAS EVER WORTH ASKING: do pixels
	// arrive? Everything above is agreement — the kernel's and the camera's —
	// and agreement has been mistaken for pixels once already in this file.
	say("")
	code := stream(ctx, h, neg, ep, size, say)
	sayDebt(h, say)
	return code
}

// stream reads frames and says whether they are pictures.
//
// ⛔⛔ A BLACK FRAME LOOKS EXACTLY LIKE A WORKING STREAM from a counter's point
// of view, and that is this package's oldest trap — the same one cmd/xrscreen
// has at the other end. So this reports the LUMINANCE SPREAD of what arrived,
// which is what go-xrkit/xrkit/headflow itself refuses to track on when it is
// zero: a frame with no structure cannot be a reference for anything.
func stream(ctx context.Context, h *android.USBHandle, neg android.UVCNegotiation,
	ep android.USBEndpoint, size android.UVCFrameSize, say func(string, ...any)) int {
	s, err := h.Stream(neg, ep, size.Width, size.Height)
	if err != nil {
		say("STEP 8 stream                FAILED: %v", err)
		return 1
	}
	defer func() { _ = s.Close() }()

	const want = 10
	deadline := time.Now().Add(15 * time.Second)
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	start := time.Now()
	var lo, hi byte = 255, 0
	got := 0
	// ⛔⛔ THE ERROR IS KEPT, BECAUSE A break THAT DROPS IT COST A MEASUREMENT.
	// The first run of this step reported "0 frame(s) in 0s — 0 packets" and the
	// verdict read that as a silent camera. It was not: Frame had failed on its
	// FIRST call and the reason went into a discarded variable. Zero packets in
	// zero seconds cannot be a camera that is merely quiet — a quiet one still
	// has its endpoint scheduled and still fills the wait.
	var stopped error
	for got < want {
		img, err := s.Frame(rctx)
		if err != nil {
			stopped = err
			break
		}
		got++
		// Sample the middle band, which is where headflow looks.
		y := size.Height / 2
		row := img.Pix[y*img.Stride:]
		for x := 0; x < size.Width; x++ {
			l := row[x*4]
			lo, hi = min(lo, l), max(hi, l)
		}
	}
	elapsed := time.Since(start)
	st := s.Stats()
	say("STEP 8 stream                %d frame(s) in %s", got, elapsed.Round(time.Millisecond))
	say("    %s", st)
	// ⛔⛔ THE CENSUS PER CHANNEL, because the totals did not close: 10 frames
	// delivered, 10 dropped, and 7968 packets — about thirty-one frames' worth
	// of data for twenty frames seen. Which boundary fired, and how far short
	// the losers were, is the difference between a camera that splits its frames
	// and a bus that loses packets.
	if tg, eo, sh := s.Assembler().Boundaries(); tg+eo+sh > 0 {
		avg := 0
		if st.Dropped > 0 {
			avg = sh / st.Dropped
		}
		say("    boundaries: %d toggle(s), %d EOF bit(s); the dropped frames were short "+
			"by %d bytes in all, %d each on average of a %d-byte frame",
			tg, eo, sh, avg, size.Width*size.Height*android.BytesPerPixelYUY2)
		onT, onE := s.Assembler().Losses()
		say("    the losses ended on: %d toggle(s), %d EOF bit(s) — whichever is the "+
			"larger is the boundary firing where it should not", onT, onE)
	}
	if stopped != nil {
		say("    STOPPED BY: %v", stopped)
		switch {
		case errors.Is(stopped, context.DeadlineExceeded):
			say("    — the wait ran out, which is the camera being slow or silent " +
				"rather than anything refusing")
		case errors.Is(stopped, syscall.ENODEV):
			say("    — ENODEV: the device went away mid-stream. On this phone that is " +
				"what unplugging looks like, and also what a host that dropped its " +
				"UsbDeviceConnection looks like")
		case errors.Is(stopped, syscall.EINVAL):
			say("    — EINVAL: the kernel accepted the submit and refuses the reap, " +
				"which points at the request's own shape rather than at the camera")
		}
	}

	if got == 0 && stopped != nil && st.Packets == 0 {
		say("ANSWER NOT A SILENT CAMERA — no packet was collected at all, in no time, "+
			"so nothing was waited for. The stream stopped on its own first call: "+
			"%v. A camera that was merely quiet would still have its endpoint "+
			"scheduled and would still fill the wait with empty packets.", stopped)
		return 1
	}
	if got == 0 {
		say("ANSWER NO PIXELS — the camera agreed to a format and delivered no whole " +
			"frame. The packet and byte counts above say whether anything arrived at " +
			"all: packets with no bytes mean the endpoint is scheduled and the camera " +
			"is silent, while bytes with no frames mean the payloads are arriving and " +
			"the assembly is wrong.")
		return 1
	}
	say("ANSWER %d WHOLE FRAMES at %.3g fps, luminance %d..%d",
		got, float64(got)/elapsed.Seconds(), lo, hi)
	if st.Late > 0 {
		// ⭐ LATE IS OURS AND DROPPED IS THE BUS'S, which is why they are
		// counted apart: the fixes are opposite. A late frame says this loop is
		// slower than the camera; a dropped one says the link loses packets.
		say("⚠ %d frame(s) WERE SHED because this loop did not come back for them in "+
			"time. That is not a lossy link — it is this process being slower than the "+
			"camera, and the frames kept are the freshest, which is what a tracker "+
			"wants.", st.Late)
	}
	if hi == lo {
		say("⛔ AND THEY ARE BLANK. Every pixel of the middle row is %d, so the stream "+
			"runs and carries no picture — which is exactly what a working one looks "+
			"like to a frame counter. headflow would refuse to track on this.", lo)
		return 1
	}
	say("RESULT the camera's pixels reach a CGO-free Go process on an unrooted phone, "+
		"with a luminance spread of %d. What remains is wiring them to "+
		"go-xrkit/xrkit/headflow, which takes an *image.RGBA and is what Decode "+
		"produces.", hi-lo)
	return 0
}

// sayDebt reports what claiming the camera took from the rest of the phone.
//
// ⛔⛔ IT IS PRINTED WHETHER THE RUN SUCCEEDED OR NOT. Claiming the streaming
// interface detaches uvcvideo, and USBDEVFS_CONNECT does not reliably put it
// back: the ioctl succeeds and the driver still does not rebind. A probe that
// left the camera half dismantled without a word would be worse than one that
// failed — and a failing run is exactly when nobody reads to the end.
func sayDebt(h *android.USBHandle, say func(string, ...any)) {
	if d := h.Detached(); len(d) > 0 {
		say("⛔ INTERFACE %v WAS TAKEN FROM uvcvideo and the reconnect does not restore "+
			"the binding on this phone: /dev/video0 survives on the control interface, "+
			"but the streaming one is left with no driver. UNPLUG AND REPLUG the headset "+
			"to give it back to the system.", d)
	}
}

// AIDCamera is Android's camera group, AID_CAMERA. /dev/video* is owned
// system:camera, so a process in this group could open it directly.
const AIDCamera = 1006

// reportV4L2 asks whether the headset's camera can be reached as a V4L2 device
// instead of as a USB one.
//
// ⭐ IT IS THE LAST ROUTE, AND IT EXISTS BECAUSE THE KERNEL ALREADY WORKS. The
// camera is driven: uvcvideo holds both its interfaces and has published
// /dev/video0. That is also WHY the USB route is shut — Android refuses an
// application a device a kernel driver owns, and usbfs, which exists to take an
// interface from a driver, is behind that same refusal.
//
// So the question becomes whether the node itself is reachable. Two things are
// measured rather than argued:
//
//   - the process's GROUPS. On older Android, android.permission.CAMERA granted
//     the camera GID, and a process holding it could open the node. If 1006 is
//     absent, no amount of permission will help;
//   - the open itself. EACCES can come from the group OR from SELinux, and the
//     two are told apart by `logcat | grep "avc: denied"`, which names the
//     class and the context.
//
// ⚠ A FAILURE HERE IS EXPECTED AND IS STILL WORTH MEASURING. SELinux almost
// certainly denies an untrusted_app on a video_device; "almost certainly" is not
// a measurement, and this is the cheapest one that settles it.
func reportV4L2(say func(string, ...any)) {
	groups, err := os.Getgroups()
	if err != nil {
		say("⚠ Getgroups: %v", err)
	}
	inCamera := false
	for _, g := range groups {
		if g == AIDCamera {
			inCamera = true
		}
	}
	say("V4L2 uid=%d groups=%v camera(%d)=%t", os.Getuid(), groups, AIDCamera, inCamera)

	for _, node := range []string{"/dev/video0", "/dev/video1"} {
		f, err := os.OpenFile(node, os.O_RDWR, 0)
		if err != nil {
			say("V4L2 %s  REFUSED: %v", node, err)
			continue
		}
		_ = f.Close()
		say("V4L2 %s  OPENED — the camera is reachable as a V4L2 device, and a pure-Go "+
			"VIDIOC_* path is open", node)
	}
	say("")
}

// streamingEndpoint picks the UVC streaming interface's alternate with the
// LARGEST isochronous IN endpoint.
//
// ⭐ THE LARGEST, because the alternates are a bandwidth ladder and the biggest
// is the one a real stream would want — and because it is the one most likely to
// be refused for bandwidth, which is a different refusal from being refused for
// permission and worth telling apart.
// frameAt finds the fastest frame of exactly the given format and width.
//
// ⚠ IT LIVES HERE AND NOT IN THE LIBRARY. A real caller wants the policy
// [android.PickFormat] implements; this is an instrument for a sweep, and a
// library that grew a knob for every experiment would be a library of knobs.
func frameAt(fs []android.UVCFormatDesc, kind android.UVCFormat, w int) (
	android.UVCFormatDesc, android.UVCFrameSize, bool) {
	for _, f := range fs {
		if f.Format != kind {
			continue
		}
		var best android.UVCFrameSize
		found := false
		for _, z := range f.Sizes {
			if z.Width != w {
				continue
			}
			// Fastest means the SHORTEST interval, which is the largest frame
			// rate: the interval is in 100ns units.
			if !found || (z.Interval > 0 && z.Interval < best.Interval) {
				best, found = z, true
			}
		}
		if found {
			return f, best, true
		}
	}
	return android.UVCFormatDesc{}, android.UVCFrameSize{}, false
}

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
