<p align="center"><img src="https://raw.githubusercontent.com/go-xrkit/brand/main/social/go-xrkit.png" alt="go-xrkit" width="720"></p>

# go-xrkit/android

[![CI](https://github.com/go-xrkit/android/actions/workflows/ci.yml/badge.svg)](https://github.com/go-xrkit/android/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-xrkit/android.svg)](https://pkg.go.dev/github.com/go-xrkit/android)
[![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)](https://github.com/go-xrkit/android/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)

Screen capture on Android from **pure Go**, `CGO_ENABLED=0`, for an XR
compositor that redraws every frame — and the two things around it that a
virtual desktop needs: **displays this application makes** ([`Wall`](#the-go-api-for-owned-displays)),
and **the glasses, painted from Go** ([`Screen`](#painting-on-the-glasses-from-go)).

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

**Half of it is no, and the other half is yes** — and the earlier edition of this
file got the boundary wrong by drawing it in the wrong place. It is not *your own
app* versus *other people's*, and it is not *virtual display* versus *real one*.
It is **an activity launch versus a window**, on **a display you made versus a
display you were given**:

| | a display you were GIVEN (external, overlay) | a display you MADE (`createVirtualDisplay`) |
|---|---|---|
| **launch an activity** on it (`setLaunchDisplayId`) | **yes** — ours *and* Settings | **no** — `SecurityException`, even for our own activity |
| **show a `Presentation`** on it | **yes** | **yes**, and the pixels arrive |

So an ordinary, unprivileged APK **cannot manufacture desktops for other people's
applications**, which is what a macOS virtual display gives
[`go-xrkit/desk`](https://github.com/go-xrkit/desk). But it **can manufacture
several hundred independent displays of its own content**, each rendered by the
real Android view system and each read back as pixels. That is a great deal more
than one mirrored phone, and it is what the Android ribbon is actually built on.

Everything below was read off a **live Android 15 (API 35, arm64) system**, not
off documentation. `host/../docs` carries no prose about this: the transcript
*is* the documentation. The probe that produced it is
[`XrDisplayProbeActivity`](host/java/org/goxrkit/android/XrDisplayProbeActivity.java),
and it is in the APK: every number below can be re-read by running it.

### Creating a virtual display and launching something on it

| what was asked for | what Android 15 answered |
|---|---|
| `VIRTUAL_DISPLAY_FLAG_PUBLIC \| PRESENTATION` | `SecurityException: Requires CAPTURE_VIDEO_OUTPUT or CAPTURE_SECURE_VIDEO_OUTPUT permission, or an appropriate MediaProjection token in order to create a screen sharing virtual display.` |
| `… \| OWN_CONTENT_ONLY` | **created** — display id 2, `flags=0x8 [PRESENTATION]`. Note what is *absent*: `FLAG_TRUSTED`. |
| launch **Settings** on it, `ActivityOptions.setLaunchDisplayId(2)` | `SecurityException: Permission Denial: starting Intent { act=android.settings.SETTINGS … } from ProcessRecord{… org.goxrkit.probe/u0a151} … with launchDisplayId=2` |
| launch **our own activity** on it | *the same denial* — an app may not put even itself on its own virtual display |
| `… \| VIRTUAL_DISPLAY_FLAG_TRUSTED` (`1 << 10`) | `SecurityException: Requires ADD_TRUSTED_DISPLAY permission to create a trusted virtual display.` |

### But a `Presentation` on that same display works, and the pixels arrive

This is the door the first edition of this file never tried, and it was sitting
in plain sight: the display it *did* create came back carrying the very flag that
says a Presentation may be shown there. A `Presentation` is a `Dialog` attached
to a `Display` — not an activity start — and the check that refused the table
above is an activity-start check.

```
Q1 created display 17 "xr-probe-own-0" real=640x480 @320dpi (density 2.0) appBounds=640x480 refresh=60.0Hz flags=0xc state=2 valid=true
Q1 ANSWER: Presentation.show() SUCCEEDED on the app's OWN virtual display 17
Q2 frame 640x480 rowStride=2560 pixelStride=4 distinctColours>=64 black=0/307200 meanRGB=85 topLeft=#FF00FF00 topRight=#FFFF0000 bottomLeft=#FFFF0000 bottomRight=#FF0000FF
Q2 ANSWER: the PIXELS ARRIVED and are the ones the Presentation drew — green top-left, blue bottom-right, red elsewhere, at the exact sampled coordinates
```

**No exception, no permission, no consent dialog, and no `MediaProjection`.**

The second line is the one that matters, because a `show()` that returns and a
buffer that arrives are two different facts, and a buffer that arrives *black* is
the third. So the probe does not look at the picture: it draws solid quadrants of
exactly known colour and **samples four coordinates**, then reports the black
count and the distinct-colour count alongside them. `black=0/307200` and four
sentinels that hold is a frame that carries what was drawn. A run where the
sentinels did not hold would be reported as a failure, not as a success.

The picture is there anyway, because a person should be able to look:
`presentation-own-virtual-display.png`, 640×480, the quadrants plus a label drawn
by Android's own text stack. It is **not committed** — see
[Where captures go](#where-captures-go).

`Display.getFlags()` answers `0xc` for that display —
`FLAG_PRESENTATION|FLAG_PRIVATE`. The `0x8 [PRESENTATION]` quoted in the table
above is `dumpsys`'s view of the underlying *device*, which does not carry the
private bit. Same display, two objects, and neither is `FLAG_TRUSTED`. **The
trusted bit gates the activity launch and nothing else here.**

### How WIDE: 32768 pixels, and the pixels arrive

The width is the whole point. [`go-xrkit/desk`](https://github.com/go-xrkit/desk)
puts a 6400-pixel spreadsheet in front of somebody on macOS; the Android ribbon
can only do the same if an owned display can BE that wide.
[`MaxDimension`](screencapture.go) says 32768, but that is this package's own
guard — not a measurement of any particular phone, and a width that is accepted
and then hands back a black buffer is the silent failure worth fearing.

Measured by [`cmd/xrwide`](cmd/xrwide) on a **Pixel 11 Pro Fold, Android 17
(API 37, arm64)**, one display per width, each wall closed before the next:

| width | pixels sampled | wrong | black |
|---|---:|---:|---:|
| 1920 x 1080 | 779 581 | **0** | 0 / 2 073 600 |
| 3840 x 1080 | 1 558 621 | **0** | 0 / 4 147 200 |
| **6400 x 1080** | 2 597 341 | **0** | 0 / 6 912 000 |
| 8640 x 1080 | 3 506 221 | **0** | 0 / 9 331 200 |
| 16384 x 1080 | 6 648 349 | **0** | 0 / 17 694 720 |
| **32768 x 1080** | **13 296 157** | **0** | **0 / 35 389 440** |

Every width opened and every width carried its content: `RESULT widest display
that carried its pixels: 32768`. Thirty-five million pixels at the widest, all
correct, and the whole sweep takes about 700 ms.

⚠ **The sweep distinguishes three answers, and the distinction is the point.**
A width can be *refused* — a limit to respect. It can be *accepted and blank* —
a limit that LIES, and the one a naive check reports as a success. Or it can
arrive with its pixels, sampled at the coordinates
[`SentinelColorAt`](sentinel.go) vouches for, which is the same yardstick
`cmd/xrwall` uses so the two commands cannot disagree about what a correct panel
looks like.

⚠ **One wall per width, closed immediately.** A 32768 x 1080 surface is 135 MB
of BGRA on its own; measuring where the edge is must not be what pushes the
process over it.

### How many at once: 304, and then the system dies

Not a graceful refusal — that is the finding, and it is the reason a product must
impose its own limit rather than discovering the platform's.

```
Q3 1920x1080 #303 id=307 OK — frame 1920x1080 rowStride=7680 pixelStride=4 distinctColours>=64 black=0/2073600 meanRGB=85 topLeft=#FF00FF00 …
E AndroidRuntime: *** FATAL EXCEPTION IN SYSTEM PROCESS: android.ui
E AndroidRuntime: android.view.Surface$OutOfResourcesException: NO_MEMORY
E AndroidRuntime: 	at android.view.SurfaceControl.nativeCreate(Native Method)
E AndroidRuntime: 	at com.android.server.wm.WindowContainer.createSurfaceControl(WindowContainer.java:678)
E AndroidRuntime: 	at com.android.server.wm.DisplayAreaPolicyBuilder$PendingArea.instantiateChildren(DisplayAreaPolicyBuilder.java:963)
E AndroidRuntime: 	at com.android.server.wm.DisplayContent.<init>(DisplayContent.java:1224)
E AndroidRuntime: 	at com.android.server.wm.RootWindowContainer.onDisplayAdded(RootWindowContainer.java:2769)
E AndroidRuntime: DeadSystemException: The system died; earlier logs will point to the root cause
```

`system_server` takes the exception, dies, and the device soft-reboots. An
ordinary APK with no permissions at all did that.

Two runs, and the number is worth reading carefully:

| displays created | display size | result |
|---|---|---|
| 304 | 640×480 | #303 fine; creating #304 killed `system_server` |
| 304 | 1920×1080 | #303 fine; creating #304 killed `system_server` — **the same count** |

**The same count at nine times the pixels**, with the app's Java heap at 14 MiB
of 192. So it is not graphics memory: it is `SurfaceControl` handles, and
`DisplayContent`'s constructor builds a whole `DisplayAreaPolicy` tree of them
per display. The ceiling therefore does **not** move if you make the screens
smaller, which is exactly the mitigation somebody would reach for.

64 displays at 1920×1080, each with a Presentation whose pixels arrived correct,
cost 14 MiB of Java heap and nothing else observable. A ribbon wants a dozen. The
useful reading is **"as many as you want, with a hard cap you set yourself"** —
and 304 is a number measured on one emulator, not a constant to hard-code.

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

**Mirroring, plus a ring of its own screens.** One panel carrying the phone's
real screen, and as many independent displays of the application's own content as
it cares to create — each a real Android display with a real view hierarchy on
it, each read back as pixels through an `ImageReader`.

What it does **not** get is other people's applications on those screens. That
half of the refusal is real and is not going anywhere: `ADD_TRUSTED_DISPLAY` is
`signature`. The ring is ours to fill.

This is the same shape as the answer on Linux, where `xrandr --setmonitor` carves
virtual monitors out of a real one with no privileged API at all.

---

## The Go API for owned displays

The probe proved the platform allows it; this is how a consumer uses it. A
`Wall` is a set of displays this application created, and the limit on how many
may exist at once.

```go
w, err := android.NewWall(0)          // 0 = DefaultMaxDisplays (8)
defer w.Close()

d, err := w.Open(ctx, android.DisplaySpec{
        Width: 1280, Height: 720,
        Content: android.Web{URL: "https://example.org/"},
})
if errors.Is(err, android.ErrTooManyDisplays) { ... }
defer d.Close()

for {
        f, fresh := d.Frame()          // BORROWED pixels, stride carried
        if fresh {
                composite(f.Pix, f.Width, f.Height, f.Stride)
        }
}
```

`OwnedDisplay` **embeds `*Stream`**, so a ribbon consumes a display it made
exactly as it consumes the mirrored phone — `Frame`, `WaitFrame`, `Stats`,
`Format`, `Size`, an idempotent `Close`. That is the point: the ribbon does not
have to know which kind of panel it is holding.

| | |
|---|---|
| `NewWall(max int) (*Wall, error)` | `0` means `DefaultMaxDisplays`; past `MaxDisplays`, or negative, is `ErrTooManyDisplays` and no wall |
| `(*Wall).Open(ctx, DisplaySpec) (*OwnedDisplay, error)` | one display, one Presentation, one feed |
| `(*Wall).Len() int`, `(*Wall).Max() int` | how full, and how full it may get |
| `(*Wall).Close() error` | releases every display it still holds; idempotent |
| `(*OwnedDisplay).ID() int` | the `android.view.Display` id, for logs and `dumpsys` |
| `(*OwnedDisplay).Spec() DisplaySpec` | the spec, with its zero fields filled in |
| `(*OwnedDisplay).Close() error` | releases the display and gives the slot back; idempotent |
| `SentinelColorAt(w, h, x, y) (uint32, bool)` | the colour a correct sentinel frame holds there, or `false` where anti-aliasing makes it unsafe to assert |

### The drawing seam: a DECLARATION, not pixels

`DisplaySpec.Content` names what Android should render. The obvious alternative
— hand the host a framebuffer to blit — was rejected, and the reason is worth
stating because it looks like the natural design:

**it would be a round trip with no product.** Pixels this process drew, sent to
Android, read back unchanged. An application that is drawing should composite in
Go and skip the display entirely.

The reason to route a ribbon panel through a real Android display is the
opposite one: to get at what Android renders and a CGO-free Go process
**cannot** — a `WebView`, a `MediaCodec` surface, a `PdfRenderer`, a maps view.
None is reachable without JNI, all are ordinary `View`s, and a `Presentation` is
how a `View` gets onto a display. So `Content` names it, the Java host builds it,
and the pixels come back through the same borrowed-frame path as a capture. It
also keeps the rule the rest of this package is built on: *a URL is a decision
the application made; a WebView is the host obeying it.*

Two implementations, and the set is closed — a `Content` written outside this
package would name a Java class the host does not have:

- **`Web{URL}`** — a `WebView`. `http`, `https` and `data:` URLs; JavaScript on,
  file access off. A network URL needs `android.permission.INTERNET`
  (`protectionLevel` normal); a `data:` URL needs nothing.
- **`Sentinel{Label}`** — flat quadrants of exactly known colour. It is the
  package's self-test content and it is exported because a consumer needs it
  too: a feed that "works" and delivers a black buffer is the silent failure of
  this whole mechanism, and the only way to catch it is to draw something whose
  pixels are known in advance and then **sample** them.

### The limit is the safety, and it refuses in both directions

`Wall` refuses past its limit with `ErrTooManyDisplays` **before asking the
host**, so a full wall costs no round trip and creates nothing. `XrWallService`
enforces `MAX_DISPLAYS = 32` independently, because a process that does not use
this API must not be able to get past it either.

| | |
|---|---|
| `DefaultMaxDisplays` | **8** — a ribbon uses six to eight |
| `MaxDisplays` | **32** — the most `NewWall` accepts however explicitly it is asked |
| measured on one emulator | **304**, at which `system_server` died and the device rebooted |

**Read this before raising the limit.** The ceiling was the *same count* at
640×480 and at 1920×1080 — nine times the pixels, same number, app heap at 14 MiB
of 192. It is `SurfaceControl` handles, not graphics memory, and
**making the screens smaller does not help**, which is exactly the mitigation
somebody reaches for. 304 is one emulator's measurement, is deliberately not a
constant anywhere in the code, and a real device's number is unknown and could
be lower.

Proved on the device, in both directions:

```
LIMIT 2 of 2 opened, as they must
LIMIT refused past 2, as it must: android: too many virtual displays: this wall holds 2 of at most 2; close one before opening another
LIMIT NewWall refuses past the ceiling of 32, as it must
```

### One connection per display

The capture host memoises a single process-wide session, because there is one
screen and one projection. A wall is the opposite: several displays live at
once, each with its own shared buffer and its own frame stream. Multiplexing
them down one socket would mean a feed id on every frame message and routing on
the hot path, where a bug mixes two panels' pixels.

A connection each costs one socket per panel — nothing, for the six to eight a
ribbon uses — and buys three things: `Stream` is reused **unchanged**, including
its tested frame plumbing; a display's lifetime is exactly a socket's lifetime,
so closing one cannot disturb another; and a client that dies has its displays
released by the kernel closing its sockets.

`XrWallService` is therefore a **second service** on its own socket
(`<package>.xrwall`). It needs no `MediaProjection`, no consent dialog and no
foreground service — **an owned display asks for no permission at all** — so
opening a ribbon panel cannot put a "recording your screen" chip in the status
bar, and a capture session ending cannot take a panel with it.

A `Presentation` shown from a **Service** context works, which was not obvious
and was measured rather than assumed: `Presentation` is a `Dialog`, and a Dialog
from a non-Activity context normally fails with `BadTokenException`. It does not
here, on a private display the app owns.

### What the live proof reports

`cmd/xrwall`, packaged into the APK and run on the device:

```
WALL available=true
WALL package default 8, ceiling 32
OPEN owned display 7 640x480 @320dpi sentinel 0
FRAME panel 0 640x480 stride 2560 seq 1: sampled 115921, wrong 0, black 0/307200
OPEN owned display 8 640x480 @320dpi sentinel 1
FRAME panel 1 640x480 stride 2560 seq 1: sampled 115921, wrong 0, black 0/307200
OPEN owned display 9 640x480 @320dpi sentinel 2
FRAME panel 2 640x480 stride 2560 seq 1: sampled 115921, wrong 0, black 0/307200
WALL holds 3 of 8
OPEN owned display 10 640x480 @320dpi web data:text/html,<body style='margin:0;background:%23FFFF00'>...
FRAME web panel 640x480 seq 2: 98.5% of the pixels are the page's own colour
RESULT every panel carried the pixels its content drew, and the limit refused in both directions
```

**115 921 pixels asserted per panel, none wrong, none black** — sampled at the
coordinates `SentinelColorAt` vouches for, never by looking at the picture. The
web panel is asserted the same way, on the page's own background colour rather
than on the absence of an error. The artefact is `wall-sentinel.png`; it is
**not committed** — see [Where captures go](#where-captures-go).

```sh
APP=./cmd/xrwall host/build.sh && adb install -r host/out/xrhost.apk
# `--ez capture false` skips the mediaProjection foreground service, which the
# platform KILLS THE PROCESS for starting without the project_media app-op.
adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity --ez capture false
adb logcat -s xrcapture xr-wall
host/pull-artifacts.sh wall-sentinel.png
```

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
| **rate** | 41.0 fps sustained at 1080×2400 on a **software-rendered emulator**, with the capturing app in the background. A second run of the same binary, against a slower-changing screen, measured 18.5 fps — the ceiling is a ceiling, and what the screen does sets the rest. A Java-only probe of the same `ImageReader` path, with no Go in it, measured 48.0 fps, which bounds this package's own overhead at a few frames a second |
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
`Options.FPS` is a **ceiling, not a rate**. Left in front of a static screen,
the same `cmd/xrcapture` binary took **35 frames in the first 0.6 seconds and
then nothing at all for the remaining eleven** — `RESULT frames=35 over 0.63s`.
That is the platform saying nothing moved; the second return value of `Frame` is
the truth about it, not a timer.

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

The display an app-created virtual display gets is `flags=0x8 [PRESENTATION]`
as a device and `0xc [PRESENTATION|PRIVATE]` as a `Display`. The system-created
secondary display is `flags=0x88 [PRESENTATION|TRUSTED]`, and the built-in panel
is `0x4083`. The trusted bit is the whole difference.

### "A screen larger than the phone's" is the glasses' own display

A competing application shows, in the glasses, a screen bigger than the phone's
panel. There is nothing clever behind it and it is worth saying so rather than
inferring a mechanism: **it is the external display, used at its own resolution.**
Nothing virtual is involved.

```
Q5 display 0  "Built-in Screen" real=1080x1920 @420dpi (density 2.625) appBounds=1080x1920 refresh=60.0Hz flags=0x4083
Q5 display 16 "Overlay #1"      real=1920x1080 @320dpi (density 2.0)   appBounds=1920x1080 refresh=60.0Hz flags=0x88
Q5 GIVEN display 16 "Overlay #1": Presentation.show() SUCCEEDED, presentation context 1920x1080 @320dpi
Q5 GIVEN display 16 after layout, decor 1920x1080
```

An ordinary app gets **the whole of it**: 1920×1080 at the display's own 320 dpi,
not the phone's 420, and the decor view really lays out at 1920×1080 — measured
after layout, not read off the metrics, because a window that reports a size and
lays out smaller is the thing worth catching. The phone stays 1080×1920 at 420
dpi and is free to show something else.

One trap, since it produced a wrong number here first: asking
`getMaximumWindowMetrics()` of a plain `createDisplayContext(d)` answers with the
**default** display's bounds. It has to be a *window* context —
`createDisplayContext(d).createWindowContext(TYPE_APPLICATION, null)` — and the
wrong answer looks entirely plausible.

### The phone as a trackpad

The device's own touchscreen keeps working normally while the content people are
looking at is somewhere else, and the Presentation does not steal the events:

```
Q4 READY: presentation up on display 146, phone touches so far 0
TOUCH phone view ACTION_DOWN (340.00195,930.0) displayId=0
TOUCH phone view ACTION_UP (340.00195,930.0) displayId=0
…
Q4 ANSWER: phone view received 16 MotionEvents while its content was on display 146;
           the presentation view received 0; last phone event ACTION_UP (620.001,1140.0)
```

Sixteen `MotionEvent`s to the view on display 0, **zero** to the Presentation's
view — which is the half that makes a trackpad possible rather than a conflict.
An app-created virtual display has no input device attached to it at all (its
`DisplayViewport` is `touch VIRTUAL`), so there is nothing to arbitrate: the
phone's digitiser belongs to the phone's window, and the pointer drawn into the
ribbon is ours to place. Nothing new is needed on the Go side —
[`go-widgets/android`](https://github.com/go-widgets/android) already forwards
touch as `MsgPointer`; what changes is only where the application chooses to draw
the cursor.

**What a headset's `Display` reports** — its `Name` is what the catalogue in
[`go-xrkit/xrkit`](https://github.com/go-xrkit/xrkit)`/glasses` matches models
on. For the built-in panel Android reports a device string ("Built-in Screen" on
this emulator); for an external sink it comes from the EDID. **No Android
artifact for any real headset exists here** — see below, and please read that
section before believing anything about a specific model.

## Painting on the glasses from Go

Everything above is about pixels coming **from** Android. This is the other
direction, and it is what a headset is actually for: the glasses are an
**output**, and what belongs on them is the ribbon the application composited.

```go
ds, err := android.Screens(ctx)          // the displays that will take a presentation
scr, err := android.ShowOn(ctx, ds[0], android.ScreenOptions{})
defer scr.Close()

for {
    c, err := scr.Next(ctx)              // a slot to draw into, waiting if all are in flight
    draw.Draw(c.RGBA(), c.RGBA().Bounds(), src, image.Point{}, draw.Src)
    err = scr.Present(ctx, c)            // hand it over; the slot comes back on the ack
}
```

`Screens` asks the **wall** host, not the capture host, and that is the
difference between needing a `MediaProjection` and needing nothing at all. An
application that only wants to paint on the glasses would otherwise have had to
start the whole projection apparatus — a foreground service, a consent dialog, a
"recording your screen" chip in the status bar — to find out that a pair of
glasses is plugged in.

It keeps only displays the platform flagged **and** that are not the built-in
panel. The flag alone is not the answer: some devices set `FLAG_PRESENTATION` on
the default display, and an application that believed it would ask for a window
over the launcher and be refused by a check it had already passed.

### The back-pressure is the point

`Next` hands out a slot, `Present` gives it to the host, and the slot comes back
only once the host says the frame reached the view. Nothing else paces the
application: the pixels go the other way here, so there is no stream of host
frames to wait on, and overwriting a slot mid-blit is a tear in a headset.

`ScreenOptions.QueueDepth` is how many slots there are, bounded by the same
`MinQueueDepth`..`MaxQueueDepth` as a capture. `Screen.Stats().Waited` counts the
times the application was faster than the glasses.

A canvas is valid **from the `Next` that returned it until the `Present` that
hands it back**, and presenting one twice is refused — including in the window
before the acknowledgement arrives, which is most of the time and is exactly
when a second `Present` gets made by mistake.

### The host's stride, not the frame's width

`Canvas.Stride` is the distance between two rows and is what the host announced.
`Canvas.RGBA()` hands back an `image.RGBA` over the **same memory**, so the whole
of `image/draw` paints a frame with no copy; `Canvas.Row(y)` gives one row's
bytes to a compositor that writes spans, with its capacity stopping at the row so
a span cannot run into the padding and corrupt the next one.

### ⛔ What an acknowledgement is, and what it is not

`cmd/xrscreen` reports how many frames the host took. That number separates "the
mechanism works" from "the mechanism runs and shows black", which is this
feature's silent failure — and it is worth having for exactly that. On a
**Pixel 11 Pro Fold** with **VITURE Beast** glasses:

```
TARGET display 13 "VITURE Beast" 1920x1080 @110dpi 60Hz (presentation)
SCREEN screen on display 13, 1920x1080, 7680-byte rows, 0 presented, 0 acknowledged, 0 waits
PAINTED 3600 frames of 1920x1080 in 2m0s, 30.0 fps
STATS 3600 presented, 3600 acknowledged, 0 waits
```

**Zero waits over two minutes.** The host stayed ahead of the application
throughout — 248 MB/s of copies through the shared buffer — so the queue depth
was never the thing limiting the frame rate. The display id is 13 here and 12 in
an earlier run: ids are per attachment and are good for logs and
`dumpsys display`, nothing more.

**It does not say a photon left the panel.** There is nothing to read back: an
ordinary application may not capture a display it does not own, so unlike
`cmd/xrwide` and `cmd/xrwall` there is no pixel to sample. The glasses are the
only instrument and the person wearing them is the only witness, which is why
what gets drawn is deliberately unmistakable — flat quadrants of known colour
with a bar sweeping across — so that one glance settles the rest.

### What the witness reported

**Colour, and movement.** The person wearing the Beast saw the quadrants, and
saw them *change*, on the run transcribed above.

Both halves were asked for separately and both matter, because they refute
different failures. Colour alone refutes a black panel — the one an
acknowledgement count cannot see. **Movement refutes a screen frozen on its
first frame**, which is what a host that showed one bitmap and then stopped
copying would look like: 3600 acknowledgements and one picture. That is the
whole reason the bar is in the frame rather than a still pattern, and it is why
it was asked about on its own after "I saw colours" came back.

This is a person's report, written down as a person's report. It is not a
measurement, it does not belong in a table with the frame counts, and it is the
best evidence that exists for this half of the package.

```sh
APP=./cmd/xrscreen host/build.sh && adb install -r host/out/xrhost.apk
adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity \
    --ez capture false --es args '-wait 20m -frames 3600 -fps 30 -hold 60s'
# unplug the cable, attach the glasses, look
adb shell run-as org.goxrkit.androidhost cat files/screen.txt
```

It **waits** for a display rather than asking once, because the phone has one
USB-C port and the glasses want all of it: the command is started over the cable
that then has to be moved. A command that read the display list at startup could
only ever report the phone's own panel.

## Following a head: the census, and the answer it gave

An XR ribbon has to know where somebody is looking, and **a VITURE Beast will
not say.** It holds its own 3DOF tracking and anchors the picture it is given
with it, but publishes no orientation at all — measured three ways on
2026-09-07 and none of them produced a number. See
[`go-xrkit/xrkit/headflow`](https://github.com/go-xrkit/xrkit), which recovers
the yaw from the headset's **camera** instead, at 1.14 % residual over a
there-and-back sweep.

On macOS that camera is an AVFoundation device and the matter ends there. On
Android it was not obvious an application could reach it at all, and there are
exactly two routes — so this package **asks** rather than guessing:

```go
cams, _ := android.Cameras(ctx)      // camera2, including LENS_FACING_EXTERNAL
devs, _ := android.USBDevices(ctx)   // every interface, down to its endpoints
route := android.ChooseRoute(cams, devs)
```

Neither needs a permission. `android.permission.CAMERA` is required to **open** a
camera and a USB permission to **open** a device; being told one is there is
free. So the census costs nobody a dialog, which is what makes it usable before
anything is decided. It is served by the wall host for exactly that reason.

### ⛔ The answer on a Pixel 11 Pro Fold: neither Android API can read it

Attaching the Beast adds **three** USB devices and **no camera2 device at all**:

```
BEFORE  2 camera(s): back 4000x3000, front 3840x2800 — 0 USB device(s)
AFTER   2 camera(s) — unchanged — and 3 USB device(s)

35ca:1102 "VITURE Microphone"        audio + 3 HID interfaces
0c45:6368 "USB 2.0 Camera" (Sonix)   UVC, 6 alternate settings
   interface 1 alt 1..6 class 0x0e/0x02  ep 0x81 in ISOCHRONOUS max 128..5120
35ca:1201 "VITURE Beast XR Glasses"  CDC-ACM + CDC data + audio + HID
   interface 1 class 0x0a            ep 0x83 in BULK 512, ep 0x03 out BULK 512
   interface 5 class 0x03            ep 0x85 in INTERRUPT 64

ROUTE USB, isochronous UVC
```

Two things decide it, and both are measurements rather than readings of the
documentation:

| | |
|---|---|
| **camera2 offers nothing** | the list is identical before and after. Supporting external USB cameras is left to the vendor's HAL, and this one does not |
| **every streaming endpoint is isochronous** | and Android's Java USB API submits control, bulk and interrupt transfers and **nothing else** — there is no isochronous request in `UsbDeviceConnection` or `UsbRequest`, which is why every UVC library on Android carries a native libusb |

So reaching those frames means **usbfs ioctls** on the descriptor
`UsbDeviceConnection.getFileDescriptor()` hands out — `USBDEVFS_SUBMITURB` with
`USBDEVFS_URB_TYPE_ISO`, which is plain syscalls and therefore reachable from
CGO-free Go, and a great deal more work than either of the other two routes.
Nothing of it is written here.

### What IS open, which is not nothing

`USBDevice.Readable` keeps the interfaces whose IN endpoints Android's own API
can carry. On the Beast that is three: the CDC-ACM control interface, its bulk
data interface, and a HID interface. **"The camera is closed" is not "there is
nothing to read"**, and a census that could not tell *unreachable* from
*reachable and silent* would be worth little — the 2026-09-07 finding is that
the headset says nothing about its orientation, not that nothing can be asked.

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

## Where captures go

A screen capture is a picture of whoever ran the probe, at work, and this
repository is public. So a capture is **never** written where it could be
committed, and a `.gitignore` does not count: `git add -f`, a fresh clone, or any
tool that does not consult it will publish the file anyway.

The probe writes its PNG to the app's external files directory on the **device**,
which is inside no repository. Pulling it to a workstation is the puller's
business, and the puller must send it somewhere durable and outside every git
work tree — `os.UserConfigDir()`-based, with an environment override, and the
chosen directory **walked up to the filesystem root looking for a `.git`, failing
if it finds one**. That is `captureDir(t)` in
[`go-macos/screencapture`](https://github.com/go-macos/screencapture), and the
refusal is the point:

```console
$ XRKIT_ARTIFACT_DIR=./testdata/artifacts host/pull-artifacts.sh …
REFUSED: …/testdata/artifacts is inside the git work tree at …
```

Nor does it go to a temporary directory. The artefact exists **so that a person
can look at it**, and `t.TempDir()` would be gone before anyone could.

## ⛔ Android 17 will not let your workstation reach the phone

Every command here is run over a cable, and the phone has **one** USB-C port that
the glasses want all of. The obvious escape is `adb` over Wi-Fi — and on Android
17 it does not work, for a reason that is not a misconfiguration and that costs
an evening to find. This is what was measured, so the next person does not have
to.

### The measurement

Pixel 11 Pro Fold, Android 17, phone and workstation on the same Wi-Fi, taken
seconds apart after a fresh reboot:

```
phone  → Mac    2/2 received, 9–57 ms
Mac    → phone  0/3                       ← silently dropped, not refused
```

And read out of the device itself:

```
$ adb shell device_config get android_core_networking \
      android.permission.flags.access_local_network_permission_enabled
true
```

[Local Network Protection](https://developer.android.com/privacy-and-security/local-network-permission)
is **mandatory from Android 17**: traffic to and from a local network address
needs `ACCESS_LOCAL_NETWORK`, *including accepting incoming TCP connections*. The
phone reaches out fine and nothing reaches in — which is exactly `adb connect`,
`adb pair`, and a laptop trying to poke at anything the phone is serving.

### What was ruled out, because the symptom has several plausible causes

| suspected | ruled out by |
|---|---|
| the access point isolating its clients | **two different access points** — a macOS Internet Sharing hotspot and a Freebox Ultra on 6 GHz — gave the identical asymmetry. Vary the AP, nothing changes: the phone is the constant |
| a VPN on the phone | `dumpsys connectivity` reports `NOT_VPN`, and the phone has only `lo` and `wlan0` |
| a firewall app | no third-party package matching one is installed |
| a closed port | every port behaves alike, open or not — and a genuinely reachable host with a closed port takes a **1-second timeout**, which the router does and the phone does not |
| Bluetooth tethering as a way round | the phone connects over Bluetooth, and macOS creates **no network interface** for it: there is no IP path at all |

⚠ **And one reading of ours was wrong, which is worth more than the ones that
were right.** Repeated attempts failed in 5 ms with `EHOSTUNREACH`, which reads
as an active ICMP rejection. It is not: it is **macOS's negative ARP cache**
short-circuiting the attempt without putting a packet on the wire. After a
reboot cleared it, the same ping took the full 4.5 seconds — a silent drop. A
failure that is *too fast* is a failure that never left the machine.

### What to do about it

**Nothing in this repository needs it.** The direction that works is phone →
workstation, and that is the direction the commands use: each one writes its
verdict with `android.SaveTranscript`, which survives the cable being somewhere
else, and `adb shell run-as <pkg> cat files/<name>.txt` collects it afterwards.

There is a device flag that governs the protection, and turning it off is a
decision for whoever owns the phone rather than something this README
recommends:

```sh
adb shell device_config put android_core_networking \
    android.permission.flags.access_local_network_permission_enabled false
```

It weakens a security protection for every application on the device, not just
for development, and on a release build it may not take effect at all without a
reboot — or at all.

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

### Reproducing all of it

```sh
# 1. an arm64 Android 15 emulator (Apple Silicon or an arm64 Linux host)
sdkmanager --install "platforms;android-35" "build-tools;35.0.0" \
           "system-images;android-35;default;arm64-v8a" "emulator"
avdmanager create avd -n xr35 -k "system-images;android-35;default;arm64-v8a"
emulator -avd xr35 -no-window -no-audio -gpu swiftshader_indirect &
adb wait-for-device

# 2. the unit suite, on the device
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go test -c -cover -coverpkg=. -o xr.test .
adb push xr.test /data/local/tmp/
adb shell 'cd /data/local/tmp && chmod +x xr.test && TMPDIR=/data/local/tmp ./xr.test'

# 3. the end-to-end capture, through a real APK
host/build.sh && adb install -r host/out/xrhost.apk
adb shell pm grant org.goxrkit.androidhost android.permission.POST_NOTIFICATIONS
adb shell appops set org.goxrkit.androidhost PROJECT_MEDIA allow   # skips the
                       # consent dialog; an adb-only shortcut, never an app one
adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity
sleep 3 && adb shell am start -a android.settings.SETTINGS    # something moving
for i in $(seq 1 26); do adb shell input swipe 540 1600 540 900 300; \
                         adb shell input swipe 540 900 540 1600 300; done
adb logcat -s xrcapture xr-host
adb pull /storage/emulated/0/Android/data/org.goxrkit.androidhost/files/android-capture.png

# 4. the virtual-display refusals, and the secondary-display results
adb shell settings put global overlay_display_devices "1920x1080/320"
adb shell pm list permissions -f | grep -A4 ADD_TRUSTED_DISPLAY
javap -classpath "$ANDROID_HOME/platforms/android-35/android.jar" \
      android.hardware.display.DisplayManager | grep VIRTUAL_DISPLAY

# 5. every Q1..Q5 transcript quoted above, from the probe in the APK.
#    A realistic panel first: a bare avdmanager AVD is 320x640 at 160 dpi.
adb shell wm size 1080x2400 && adb shell wm density 420
adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDisplayProbeActivity
#    … and while it prints "Q4 READY", inject taps on the PHONE's touchscreen:
adb shell input tap 540 1200
adb logcat -s xr-probe
host/pull-artifacts.sh presentation-own-virtual-display.png

# 6. the ceiling. WARNING: this one KILLS system_server and reboots the device.
adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDisplayProbeActivity \
    --es smallCap 512 --es bigCap 400
```

The probe takes `--es smallCap N` and `--es bigCap N` so the ceiling can be
hunted without rebuilding, and defaults both to **64** — comfortably below the
number that kills the device, because a probe whose default reboots the machine
is a probe nobody runs twice.

### Hardware connected and exercised

A **Pixel 11 Pro Fold**, Android 17 (API 37, arm64), with **VITURE Beast**
glasses on its USB-C port. What that device answered, and nothing more:

- the glasses are an ordinary Android display —
  `id 12 "VITURE Beast" 1920x1080 @110dpi 60.0Hz flags 0x8088 presentation true`,
  named from the sink's own EDID;
- owned displays go to **32768×1080**, with 0 wrong and 0 black pixels over
  35 389 440 sampled — see [`cmd/xrwide`](cmd/xrwide);
- `startForeground` with `mediaProjection` and no consent **kills the process**
  on API 37, which is why the host only goes foreground once consent exists;
- and the glasses take a `Presentation` carrying pixels Go painted —
  [`cmd/xrscreen`](cmd/xrscreen), **3600 frames of 1920×1080 in 2m0s at 30.0 fps,
  3600 acknowledged, 0 waits.** The queue never emptied: 248 MB/s of copies
  through the shared buffer without once making the application wait for a slot.
  The person wearing them [saw colour and saw it
  move](#what-the-witness-reported), which is the only evidence this half of the
  package can have;
- and the headset's CAMERA is reachable through neither Android API on this
  phone — no external camera2 device, every UVC streaming endpoint isochronous.
  See [the census](#following-a-head-the-census-and-the-answer-it-gave).

```
TARGET display 13 "VITURE Beast" 1920x1080 @110dpi 60Hz (presentation)
PAINTED 3600 frames of 1920x1080 in 2m0s, 30.0 fps
STATS 3600 presented, 3600 acknowledged, 0 waits
```

Everything *else* below was measured on the **Android emulator**, API level 35
(Android 15), `system-images/android-35/default/arm64-v8a`, running on an Apple
Silicon Mac — fingerprint
`Android/sdk_phone64_arm64/emu64a:15/AE3A.240806.019/12368160:userdebug/test-keys`,
1080×2400 at 420 dpi. A real arm64 Android system, a real MediaProjection, a
real `SharedMemory`, a real socket, real frames. It is not a real phone — and
where a figure below comes from the emulator rather than from the Fold, it says
so.

### Partially observed

- **The ceiling of 304, and the crash.** Reproduced twice, at two sizes, on
  **one** emulator with a software renderer. The *shape* of the failure — no
  refusal, `system_server` dies of `SurfaceControl` exhaustion, the count does
  not move with the display size — is the finding and should carry. The
  **number** is this emulator's and must not be hard-coded anywhere.
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

- **most of the capture figures are still the emulator's** — the Fold answered
  the display, wide-display, API 37 and camera-census questions; the frame rates, the ceiling
  of 304 and the consent flow were not re-measured on it, and the section above
  says which is which;
- **capture of a second display is impossible** for an unprivileged app and is
  reported as `ErrNotCapturable` rather than attempted. `MediaProjection`
  mirrors the default display; anything else needs `CAPTURE_VIDEO_OUTPUT`,
  which is `signature`;
- **the workstation cannot reach the phone over Wi-Fi on Android 17**, so every
  measurement here goes over the one USB-C port the glasses also want. See
  [above](#-android-17-will-not-let-your-workstation-reach-the-phone): it is
  Local Network Protection, it is mandatory from Android 17, and the commands
  work around it by writing transcripts rather than by being reachable;
- **the headset's camera needs usbfs, and none of that is written.** The census
  says the route and stops there: no external camera2 device on this phone, and
  every UVC streaming endpoint isochronous, so the frames are behind
  USBDEVFS_SUBMITURB on the descriptor Android hands out. Head tracking on
  Android is therefore NOT implemented, and the measurement says why rather than
  the plan saying when;
- **nobody has read a pixel back off the glasses, and nobody can.** An ordinary
  application may not capture a display it does not own, so
  [`Screen`](#painting-on-the-glasses-from-go) can report how many frames the
  host took and nothing more. The transport is measured end to end against a
  real socket and a real shared mapping — what the application draws is asserted
  to be the bytes the host finds — but between the host's bitmap and the panel
  there is only [a person looking](#what-the-witness-reported), and that will not
  change: the gap is in what Android permits, not in what is written here;
- **only two `Content` kinds exist.** `Web` and `Sentinel`. `MediaCodec`,
  `PdfRenderer` and a maps view are the obvious next ones and none is written;
- **the limit is 32 by construction, not by measurement on hardware.** It is
  enforced in both the Go API and the Java host, and it is far below the 304
  that killed `system_server` on an emulator. What a real phone does at 32 owned
  displays has not been measured, and neither has what it does at 304;
- **the trackpad is a finding, not a feature.** That the phone's `MotionEvent`s
  keep arriving while the content is elsewhere, and that the Presentation gets
  none of them, is measured. The pointer that a ribbon would draw from them does
  not exist yet;
- **the row stride is measured, not guessed — but there is a fallback.** The
  host waits up to two seconds for its first `Image` to learn the allocator's
  real stride. If none arrives (a screen that is off, say) it announces an
  aligned upper bound instead, and refuses — loudly, with `MsgStopped` — any
  later frame that would not fit. That path has not been provoked on a device.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
