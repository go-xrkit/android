# go-xrkit/android

[![ci](https://github.com/go-xrkit/android/actions/workflows/ci.yml/badge.svg)](https://github.com/go-xrkit/android/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-xrkit/android.svg)](https://pkg.go.dev/github.com/go-xrkit/android)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-xrkit/android)](https://goreportcard.com/report/github.com/go-xrkit/android)
[![coverage 100%](https://img.shields.io/badge/coverage-100%25-brightgreen)](#testing)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD_3--Clause-blue.svg)](LICENSE)

Screen capture on Android from **pure Go**, `CGO_ENABLED=0`, for an XR
compositor that redraws every frame.

```go
if !android.Authorized() {
        ok, err := android.RequestAuthorization(ctx) // the system consent dialog
        ...
}
d, _ := android.DefaultDisplay(ctx)

s, err := android.CaptureDisplay(ctx, d, android.Options{FPS: 60}) // a CEILING
if err != nil {
        return err
}
defer s.Close()

for {
        f, fresh := s.Frame() // BORROWED pixels, no copy, no allocation
        if fresh {
                composite(f.Pix, f.Width, f.Height, f.Stride) // RGBA, stride carried
        }
        render()
}
```

It is the Android member of a family — [`go-macos/screencapture`](https://github.com/go-macos/screencapture),
`go-freedesktop/screencast`, `go-mswin/screencapture` — deliberately shaped so
one consumer drives all of them through near-identical adapters: `Displays`,
`CaptureDisplay`, `Stream.Frame() (Frame, bool)` lending borrowed pixels with a
stride carried and never assumed, an idempotent `Close`.

---

## Read this first: what Android will and will not allow

The interesting question for an XR virtual desktop is not "can I capture the
screen" — it is "can I create extra desktops and put applications on them", the
way [`go-xrkit/desk`](https://github.com/go-xrkit/desk) does on macOS with real
virtual displays.

**The answer is no, and it is not close.** An ordinary, unprivileged APK may
mirror the screen it already has, and may draw its own content on a display that
is already attached. It may not manufacture desktops.

Everything below was read off a **live Android 15 (API 35, arm64) system**, not
off documentation. `host/../docs` carries no prose about this: the transcript
*is* the documentation.

### Creating a virtual display and launching something on it

| what was asked for | what Android 15 answered |
|---|---|
| `VIRTUAL_DISPLAY_FLAG_PUBLIC \| PRESENTATION` | `SecurityException: Requires CAPTURE_VIDEO_OUTPUT or CAPTURE_SECURE_VIDEO_OUTPUT permission, or an appropriate MediaProjection token in order to create a screen sharing virtual display.` |
| `… \| OWN_CONTENT_ONLY` | **created** — display id 2, `flags=0x8 [PRESENTATION]`. Note what is *absent*: `FLAG_TRUSTED`. |
| launch **Settings** on it, `ActivityOptions.setLaunchDisplayId(2)` | `SecurityException: Permission Denial: starting Intent { act=android.settings.SETTINGS … } from ProcessRecord{… org.goxrkit.probe/u0a151} … with launchDisplayId=2` |
| launch **our own activity** on it | *the same denial* — an app may not put even itself on its own virtual display |
| `… \| VIRTUAL_DISPLAY_FLAG_TRUSTED` (`1 << 10`) | `SecurityException: Requires ADD_TRUSTED_DISPLAY permission to create a trusted virtual display.` |

`VIRTUAL_DISPLAY_FLAG_TRUSTED` is not merely undocumented — it is **not in the
public SDK at all**. `javap` on the API 35 `android.jar` lists exactly five
flags, and TRUSTED is not among them:

```console
$ javap -classpath $ANDROID_HOME/platforms/android-35/android.jar \
        android.hardware.display.DisplayManager | grep VIRTUAL_DISPLAY
  public static final int VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR;
  public static final int VIRTUAL_DISPLAY_FLAG_OWN_CONTENT_ONLY;
  public static final int VIRTUAL_DISPLAY_FLAG_PRESENTATION;
  public static final int VIRTUAL_DISPLAY_FLAG_PUBLIC;
  public static final int VIRTUAL_DISPLAY_FLAG_SECURE;
```

And the permissions that would unlock it are, on the same device:

```console
$ adb shell pm list permissions -f
  permission:android.permission.ADD_TRUSTED_DISPLAY   protectionLevel:signature|role
  permission:android.permission.CAPTURE_VIDEO_OUTPUT  protectionLevel:signature
  permission:android.permission.CREATE_VIRTUAL_DEVICE protectionLevel:internal|role
```

`signature` means "signed with the platform key". `internal|role` cannot even be
granted by signature. None of the three is reachable by an APK you or I can
install.

### `VirtualDeviceManager` (Android 14+) does not change the answer

It is the obvious thing to hope for, and it is closed twice over. The public SDK
class carries **no `createVirtualDevice` at all** — only read-only queries:

```console
$ javap … android.companion.virtual.VirtualDeviceManager
  public java.util.List<VirtualDevice> getVirtualDevices();
  public VirtualDevice getVirtualDevice(int);
  public void registerVirtualDeviceListener(…);
  public void unregisterVirtualDeviceListener(…);
```

The method does exist on the platform class, so it was called reflectively to
get the platform's own answer rather than the header's:

```
vdm: createVirtualDevice EXISTS on the platform class:
     public VirtualDeviceManager$VirtualDevice createVirtualDevice(int, VirtualDeviceParams)
vdm: createVirtualDevice REFUSED:
     java.lang.SecurityException: Access denied, requires: android.permission.CREATE_VIRTUAL_DEVICE
```

### So what an XR application on Android actually gets

**Mirroring, plus its own content.** One ribbon carrying the phone's real screen
and whatever the application draws itself — not a ring of independent desktops.
That is a smaller feature than the macOS one, and pretending otherwise would
only produce an elaborate structure around something the platform refuses.

---

## What a capture delivers

Measured, on the emulator named at the bottom of this file, with the
`cmd/xrcapture` binary packaged into the APK:

```
DISPLAY display 0 "Built-in Screen" 1080x2400 @420dpi 60Hz (default)
capture 1080x2400 stride 4320 slots 3 ceiling 60.0 fps
lent 3 slots of 10368000 bytes
FIRST frame 1080x2400 stride 4320 (width*4 = 4320, padding 0) seq 1
RESULT frames=492 over 11.99s = 41.0 fps; identical-to-previous=68/491
RESULT Frame(): 22.8 ns/op, 0.000 allocations/op over 200000 calls
RESULT stats {Frames:493 Superseded:1 Interval:16.755125ms}
```

| | |
|---|---|
| **size** | the display's native pixels, 1080×2400 here — no downscale is imposed |
| **format** | `RGBA_8888` (`android.graphics.PixelFormat` 1), packed, 4 bytes per pixel |
| **stride** | 4320 bytes, which happens to be exactly `width*4` on this device. The API carries the number anyway, because a graphics allocator is free to pad and a sheared image is the classic result of assuming it does not |
| **rate** | 41 fps sustained at 1080×2400 on a **software-rendered emulator**, with the capturing app in the background. A second run measured 48 fps |
| **`Frame()`** | **22.8 ns and 0.000 allocations** per call, over 200 000 calls on the device. That is the borrow: `Frame.Pix` aliases the shared mapping the host wrote into |

### The content really changes

A capture that hands back a frozen buffer looks exactly like a working one until
somebody hashes it, so `cmd/xrcapture` hashes every frame: **68 of 491 frames
were identical to their predecessor and 423 were not**, while another
application scrolled in front. A run where every frame matched would be reported
as a failure, not as a capture.

One frame is saved as a PNG artefact:
[`testdata/artifacts/android-capture.png`](testdata/artifacts/android-capture.png)
— 1080×2400, the Settings app scrolling, taken while *this* app was in the
background. The cast indicator in the status bar is Android's own "something is
recording your screen" chip.

### Frames only arrive when something changes

Like ScreenCaptureKit on macOS, a `MediaProjection` is change-driven and
`Options.FPS` is a **ceiling, not a rate**. On a motionless screen the same
probe took **39 frames in 4.0 seconds and then nothing at all**. That is the
platform saying nothing moved; the second return value of `Frame` is the truth
about it, not a timer.

### Going to the background does not stop it

This matters more here than in most apps, because *our* app is the thing being
covered. The probe deliberately calls `moveTaskToBack(true)` the instant consent
is granted, and then captures another application for twelve seconds:
`onCapturedContentVisibilityChanged(visible=true)`, 492 frames, no interruption.

What is required for that is a **foreground service of type `mediaProjection`**,
and from API 34 it is required anyway: the platform refuses to issue a
projection token to a process that is not already running one.
`FOREGROUND_SERVICE_MEDIA_PROJECTION` is `protectionLevel:normal`, so an
ordinary APK simply declares it.

### Consent, every session

`MediaProjectionManager.createScreenCaptureIntent()` must be answered by the
user, the token is **single-use**, and nothing remembers the answer — unlike a
macOS TCC grant. `Authorized()` therefore answers "right now", not "ever", and
`RequestAuthorization` exists as a separate call because the dialog takes over
the screen and an application should choose when that happens.

---

## Reaching the glasses

On a phone with XR glasses on USB-C DP Alt Mode, Android sees an ordinary second
`Display`. Two routes were exercised against a trusted, public secondary display
(see the honesty note below about which one):

- **`Presentation`** — a window of ours on the other display while the phone
  shows something else. `Presentation.show()` succeeded, reporting
  `presentation display=8 context size=1920x1080 dpi=320` — the whole display —
  and `dumpsys window` confirms it from the outside:
  `Display: mDisplayId=8 … mObscuringWindow=Window{961b015 u0 org.goxrkit.probe}`.
- **`ActivityOptions.setLaunchDisplayId`** — this one is the surprise. Launching
  **our own** activity worked, and so did launching **Settings**:

  ```
  Display #8 (activities from top to bottom):
      topResumedActivity=ActivityRecord{… com.android.settings/.homepage.SettingsHomepageActivity}
  Display #0 (activities from top to bottom):
      topResumedActivity=ActivityRecord{… org.goxrkit.probe/.Probe}
  ```

  The framework's rule is "anyone may launch on a public display"; the
  restriction that bites in the table above applies to *virtual* displays that
  are not trusted. So on the **one** display the glasses provide, an ordinary
  app can place other applications. It still cannot make a second one.

The display an app-created virtual display gets is `flags=0x8 [PRESENTATION]`.
The system-created secondary display is `flags=0x88 [PRESENTATION|TRUSTED]`, and
the built-in panel is `0x4083`. The trusted bit is the whole difference.

**What a headset's `Display` reports** — its `Name` is what the catalogue in
[`go-xrkit/xrkit`](https://github.com/go-xrkit/xrkit)`/glasses` matches models
on. For the built-in panel Android reports a device string ("Built-in Screen" on
this emulator); for an external sink it comes from the EDID. **No Android
artifact for any real headset exists here** — see below, and please read that
section before believing anything about a specific model.

## Input

Nothing new is needed, and that is the finding rather than an omission. A
Bluetooth or USB keyboard reaches an Android app as ordinary `KeyEvent`s, which
[`go-widgets/android`](https://github.com/go-widgets/android) already forwards
to the Go process as `MsgKey` (Android key code plus the rune its key-character
map produced), alongside touch and wheel events. A ribbon scrolled from the
keyboard therefore works through the existing back-end with no capture-specific
code at all. The emulator enumerates a `KEYBOARD | ALPHAKEY | DPAD` device, so
key injection reaches an app there too.

---

## How it is put together

Android hands no `MediaProjection`, and no drawable surface, to a process that
is not the app: both are behind JNI, and JNI needs cgo. So the app is two
processes, exactly as `go-widgets/android` established:

| | |
|---|---|
| **Java host** (`host/`, ~600 lines over three files) | owns the projection token, the `ImageReader` and the mirroring `VirtualDisplay`. Forwards bytes. Decides nothing. |
| **Go application** | an ordinary `CGO_ENABLED=0 GOOS=android` executable. Owns which display, which size, which rate, and when to stop. |

### Why a separate repository, and a Service rather than an Activity

An APK may hold many components but **only one of them may own the drawing
surface**, and in a go-widgets application that one is `go-widgets/android`'s
`GwHostActivity`. Screen capture also has no business living inside a widget
toolkit. So this is its own module, and its host is:

- a **`Service`** — which is mandatory anyway from API 34, as above;
- a transparent **`Activity`** used for nothing but holding the consent dialog,
  because consent is an activity result and nothing else.

The two hosts coexist as components of one package, each owning what it must,
and the **single** Go process talks to both over two sockets. Neither host knows
the other exists.

That composition has one practical wrinkle, and it is solved here rather than by
changing go-widgets: the process is spawned by *go-widgets'* Activity, which
knows nothing about capture and sets no socket name for it. Android gives a
spawned process no way to ask its own package name either. What it does give it
is `HOME` — which that host sets to `/data/user/0/<package>/files` — so
[`android.DeriveSocket`](protocol.go) reads the package out of it and derives the
host's socket name. `XR_ANDROID_SOCKET` overrides it when something does name
one.

```
  MsgStart ─────────────────────► XrHostService ──► MediaProjection
                                        │             │
  MsgConfig ◄─── stride, slots ─────────┤             ▼
  MsgBuffer ◄─── SCM_RIGHTS fd ─────────┤          ImageReader
                                        │             │
  Frame() ◄── mmap(PROT_READ) ◄── SharedMemory ◄── one memcpy
                                        │
  MsgFrame{seq, slot, w, h, stride} ◄───┘
```

### The frame buffer belongs to the HOST, and that is forced

The natural design — the Go side creates a `memfd` and lends it to the host, the
way `go-widgets/android` lends its framebuffer — **does not work in this
direction**, because the host has to *write*. An Android app cannot map a
descriptor it received read-write. All three routes were tried on the device:

| route | Android 15's answer |
|---|---|
| `SharedMemory.fromFileDescriptor(pfd)` on a Go memfd | `IllegalArgumentException: FileDescriptor is not a valid ashmem fd` |
| `new RandomAccessFile("/proc/self/fd/" + fd, "rw")` | `FileNotFoundException: /proc/self/fd/112: open failed: EACCES (Permission denied)` |
| `FileInputStream`/`FileOutputStream` `.getChannel().map(READ_WRITE, …)` | `NonWritableChannelException` — each of those channels is open one way only |

So the host creates the region itself with `SharedMemory.create()` — ashmem,
which dirties **no page cache at all**, so tens of megabytes a second of pure
scratch never reach flash — and lends the descriptor to the application, which
maps it `PROT_READ`. A stray write on the Go side traps instead of silently
corrupting a frame under the compositor.

`SharedMemory` keeps its descriptor to itself, but `writeToParcel` is public and
writes exactly that descriptor, so a `Parcel` is the supported way back to one.

### Three slots, and why not two

`Frame()` lends; the host keeps producing. With two slots a host that produced
two frames while the consumer held one would overwrite the borrow under it. The
default and the minimum are therefore **three**, which is the same reasoning —
and the same number — behind `go-macos/screencapture`'s `MinQueueDepth`. A slot
is a whole framebuffer (10.4 MB for a 1080×2400 phone), so `MaxQueueDepth` is a
real memory limit rather than a formality.

### One copy per frame, and there is no zero-copy path

The `ImageReader` plane is gralloc memory. Mapping that needs the graphics HAL,
which is behind cgo — the one thing this stack must not have. So there is
exactly one copy: direct `ByteBuffer` to direct `ByteBuffer`, which is a native
`memcpy` with no Java heap in the middle and no allocation per frame. Measured
on a 1080×2400 frame (10.37 MB): **0.325 ms**, about 32 GB/s.

Everything after that is free: the Go side maps the same pages and `Frame()`
returns a subslice.

---

## Building the APK

No Gradle and no Kotlin: the host is three Java files and the application is a
Go binary, so the SDK's own tools are the whole tool chain.

```sh
export ANDROID_HOME=... JAVA_HOME=...
sdkmanager --install "platforms;android-35" "build-tools;35.0.0"
host/build.sh                                # → host/out/xrhost.apk
adb install host/out/xrhost.apk
adb shell pm grant org.goxrkit.androidhost android.permission.POST_NOTIFICATIONS
adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity
adb logcat -s xrcapture xr-host
```

`APP_BIN=/path/to/binary PACKAGE=org.example.app host/build.sh` packages an
application from another module; the flags are the same ones
`go-widgets/android`'s script takes, because it is the same script.

`arm64` is the only CGO-free Android architecture — `android/arm`,
`android/amd64` and `android/386` all answer *"requires external (cgo) linking,
but cgo is not enabled"* — and CI asserts both halves of that, so the day Go
lifts the restriction is a red build rather than a silent one.

## Testing

`100.0%` statement coverage, gated in CI on **both** lanes — the Linux transport
and the off-Android stub. A stub nobody tests is a stub that panics the day
somebody links it.

The transport is `//go:build linux`, and Android *is* Linux, so the suite runs
against a **fake host over a real socket, a real memfd and a real SCM_RIGHTS
handover** — not a mock:

```sh
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go test -c -cover -coverpkg=. -o xr.test .
adb push xr.test /data/local/tmp/ && adb shell /data/local/tmp/xr.test
```

Two tests are worth naming because they assert things a reading of the code
cannot:

- `TestFrameIsBorrowedNotCopied` writes through the *host's* mapping and reads it
  back out of a `Frame` the consumer already holds. The whole no-allocation
  promise rests on that frame not being a copy.
- `TestStrayDescriptorsAreNotLeaked` throws eighty misplaced file descriptors at
  the client and then counts the process's own `/proc/self/fd`. A leaked
  descriptor is invisible until the process runs out of them, so it is measured
  rather than reasoned about.

`-race` runs on the Linux lane only: the race detector needs cgo, which is the
very thing an Android application binary must not have.

---

## What was actually tested against

This section distinguishes three things and never rounds upward.

### Hardware connected and exercised

**None.** No XR glasses were attached to an Android device for any of this. That
is the honest headline and it should be read before anything else here.

Everything above was measured on the **Android emulator**, API level 35
(Android 15), `system-images/android-35/default/arm64-v8a`, running on an Apple
Silicon Mac — a real arm64 Android system, a real MediaProjection, a real
`SharedMemory`, a real socket, real frames. It is not a real phone, and it is
certainly not a phone with glasses on its USB-C port.

### Partially observed

- **A secondary display.** The `Presentation` and `setLaunchDisplayId` results
  come from the emulator's simulated secondary display
  (`settings put global overlay_display_devices "1920x1080/320"`), which reports
  `type=OVERLAY`, `uniqueId="overlay:1"`, `FLAG_PRESENTATION|FLAG_TRUSTED`. A
  display attached over DP Alt Mode is `type=EXTERNAL` and is likewise public
  and trusted, and the framework's launch check keys on *virtual versus not* and
  on private/trusted rather than on the type — so the result should carry. It
  was not verified on a real external display, and "should carry" is not
  "verified".
- **Frame rates.** 41–48 fps at 1080×2400 was measured on a software-rendered
  emulator. A device with a GPU-composited surface should do better, but this
  repository has not measured one.

### Known only from documentation

- Every published claim about a specific headset — display name, USB identity,
  the modes it offers — including everything in
  [`go-xrkit/xrkit`](https://github.com/go-xrkit/xrkit)`/glasses`. **There is no
  Android artifact for any real headset in this repository at all.** A macOS
  display name is not an Android one, and neither is an EDID read on Linux.
- That VITURE's own SpaceWalker uses this route on a phone over DP Alt Mode.

### Send us hardware

We will gladly add and verify a model we can hold. **If you want a device
supported and verified rather than quoted, send us the hardware** and it will be
tested against and listed here. Failing that, the next best thing is to plug one
in yourself and tell us what `adb shell dumpsys display` said.

## Known gaps

Deliberate, and stated rather than hidden:

- **no real device, and no real glasses** — see above;
- **capture of a second display is impossible** for an unprivileged app and is
  reported as `ErrNotCapturable` rather than attempted. `MediaProjection`
  mirrors the default display; anything else needs `CAPTURE_VIDEO_OUTPUT`,
  which is `signature`;
- **`Presentation` is not wired into this package.** Drawing on the glasses is
  the widget back-end's job, not the capture package's; what this repository
  contributes is the measured fact that the route exists and what it costs;
- **the row stride is measured, not guessed — but there is a fallback.** The
  host waits up to two seconds for its first `Image` to learn the allocator's
  real stride. If none arrives (a screen that is off, say) it announces an
  aligned upper bound instead, and refuses — loudly, with `MsgStopped` — any
  later frame that would not fit. That path has not been provoked on a device.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
