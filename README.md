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

⭐⭐ **On Android it now does too, measured**: a worn left-right sweep came back
as 197.9° of travel — out to −52°, back through zero, out to +80° — through
usbfs on an unrooted phone. See the stream section below for the whole chain and
for what it costs.

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

### ⭐ usbfs WORKS, on an unrooted phone

The census said the frames are behind usbfs, so `cmd/xrusb` goes and asks. It
takes four steps and reports which one the kernel stopped at: open, read the
descriptors, claim the interface, select the alternate, and then submit one
isochronous request — the thing `UsbDeviceConnection` cannot do at all.

Measured on a **Pixel 11 Pro Fold, Android 17, no root**:

```
TARGET usb /dev/bus/usb/001/003 0c45:6368 "Sonix Technology Co., Ltd." "USB 2.0 Camera"
STEP 1 open                  OK — the host lent the kernel's descriptor
STEP 2 read descriptors      OK — 1424 bytes, 0c45:6368 class 0xef/0x02/0x01
       streaming on interface 1 alternate 6, ep 0x81 in isochronous max 5120 interval 1
STEP 3 claim interface 1     OK — taken from whatever driver held it
STEP 4 select alternate 6    OK — bandwidth reserved

ANSWER YES — the kernel ACCEPTED an isochronous request on endpoint 0x81
```

⚠ **Accepted is not streaming**, which is why this step is only a step: the probe
submits one request and cancels it, and that says the kernel takes the call, not
that pixels arrive.

⭐ **What this paragraph used to say was missing is now written and measured** —
the probe/commit negotiation, frame assembly from the packet descriptors, and
decoding both YUY2 and MJPEG. A worn head turn comes back as 197.9° of travel;
see [the stream](#the-stream-reaping-assembling-decoding) below. The distinction
is kept rather than deleted, because it held for four releases: *accepted* was
true here long before *arriving* was, and reading one as the other is the mistake
this whole file is a record of.

### ⛔⛔ Three walls were reported here as platform limits. All three were OUR BUGS

This section said, at length and with measurements beside it, that no route
existed and that head tracking needed a privileged component. **That was wrong,
three times over**, and each mistake has the same shape: an operation was never
attempted, and the thing that stood in for it was believed.

| what was written | what it actually was |
|---|---|
| *"`UsbManager` will not hand over the descriptor"* | **`openDevice` was never called.** The code gated on `hasPermission`, which answers `false` on this phone while `dumpsys usb` records the grant for the same uid and the same node. A predicate that disagrees with the operation is a predicate, not a verdict |
| *"`permission denied` reading the descriptors"* | the descriptors were read by **reopening** `/proc/self/fd/N`. Reopening `/dev/bus/usb/…` is exactly what an application may not do — and the whole point of being handed a descriptor is that the open already happened |
| *"the kernel refuses isochronous transfers"* | `EMSGSIZE`. **`wMaxPacketSize` is not a size** on a high-speed isochronous endpoint: bits 10:0 are the packet, bits 12:11 the additional transactions per microframe. `5120` is `0x1400` → `1024 × 3 = 3072`. The raw field was passed, and the kernel said *"message too long"* — which was printed as a refusal |

And `EBUSY` on the claim — the kernel's own `uvcvideo` holding the interface —
is answered by `USBDEVFS_DISCONNECT`, which needs no privilege and is what
libusb's `detach_kernel_driver` does. `USBHandle.Close` reconnects it, because a
detached interface takes `/dev/video0` away from the whole phone.

**⛔ The probe itself told the lie, and that is the worst of it.** It printed
*"the kernel refused"* for `EMSGSIZE`. It now switches on the errno: `EPERM` and
`EACCES` are verdicts, everything else is a malformed request, and it says
which.

### What that costs to believe again

Every claim in this file about something being impossible was made with a
measurement beside it, and three of them were still wrong — because the
measurement answered a question about **our code** and was read as an answer
about the platform. Before writing that a platform forbids something:

1. **attempt the operation.** A capability predicate is a hint; the call is the
   authority. `hasPermission` said no and `openDevice` said yes;
2. **read the errno, not the failure.** `EPERM` is a refusal. `EMSGSIZE`,
   `EINVAL`, `ENODEV` and `EBUSY` are all statements about the request;
3. **look for a working third party.** Play Store apps read UVC cameras on
   unrooted phones. That fact was available the whole time and would have
   refuted the conclusion on its own.

### ⭐ And the reason the descriptor is never handed over: the KERNEL OWNS IT

The camera is not broken, unsupported or asleep. It is DRIVEN:

```
/dev/video0  crw-rw----  system camera
1-1.1:1.0  driver=uvcvideo      # the UVC control interface
1-1.1:1.1  driver=uvcvideo      # the streaming interface
video0 -> …/usb1/1-1/1-1.1/1-1.1:1.0/video4linux/video0
```

The kernel's own `uvcvideo` holds both interfaces and has published a V4L2 node
for them. Android refuses an application a USB device a kernel driver owns,
which is why `requestPermission` answers without asking — and it is **circular**:
`usbfs` exists precisely to take an interface away from a driver, and it is
behind that same refusal. The microphone (`snd-usb-audio`) and the glasses' own
HID and CDC interfaces are in exactly the same position.

### And the V4L2 node is not reachable either, for a plainer reason

```
V4L2 uid=10350 groups=[9997 20350 50350] camera(1006)=false
V4L2 /dev/video0  REFUSED: open /dev/video0: permission denied
```

With `android.permission.CAMERA` **granted**, the process is still not in the
`camera` group: the old mapping from that permission to AID_CAMERA is gone. The
node is `system:camera`, so the ordinary Unix check refuses — and it refuses
FIRST. There is no `avc: denied` for a `video_device` in the log at all, so
SELinux was never consulted. "SELinux would block it" was the prediction; the
measurement is simpler and more final than that.

### The four routes, and which one works

| route | state |
|---|---|
| **camera2** | **shut.** No external-camera HAL publishes the device; the list is identical before and after |
| **UsbDeviceConnection** | **shut.** Every streaming endpoint is isochronous, which the Java API cannot submit |
| **/dev/video0** | **shut.** Owned `system:camera`, and `CAMERA` grants no supplementary group any more |
| **usbfs** | ⭐ **OPEN.** `openDevice` hands the descriptor over, `USBDEVFS_DISCONNECT` takes the interface from `uvcvideo`, and the kernel accepts an isochronous request |

The first three are measured refusals and stand. The fourth is the one that
works, and it took three self-inflicted walls to find.

### The camera agrees to a format: 640×480 YUY2 at 25 fps

```go
formats, _ := desc.UVCFormats(iface.Number)   // the class descriptors, which no
                                              // general USB parser reports
want, size, _ := android.PickFormat(formats)
neg, _ := h.Negotiate(iface.Number, want, size)
```

Measured on the device:

```
STEP 5 read formats          OK — 2 format(s) on interface 1
STEP 6 pick                  YUY2 640x480 @25fps (interval 400000)
STEP 7 negotiate             OK — format 2 frame 6 interval 400000 (25fps) payload 2400
```

⛔ **`Negotiate` returns what the camera AGREED TO, not what it was asked.** A
UVC device may answer a probe with another frame or another interval and the
host is expected to accept it; `UVCNegotiation.Changed` says when that happened.
Reporting the request as the outcome is how a stream comes out at a rate nobody
chose. This camera agreed exactly, so that branch is exercised against a **fake**
one in the suite — the real device could not reach it.

### ⛔⛔ And the frame was chosen the wrong way round first

`PickFormat` took the **largest** frame, reasoning that more pixels across means
more to match a column profile on. The camera's own table says what that costs:

| | |
|---|---|
| **MJPEG** | **30 fps at every size**, 1920×1080 down to 160×120 |
| **YUY2** | 1920×1080 @5 fps · 1280×720 @10 · 800×600 @20 · 640×480 @25 |

So "largest" chose 1920×1080 at **five frames a second** — 200 ms a frame, worse
than the 46–107 ms the same algorithm already achieves on macOS, for a precision
nothing here uses. **Latency is what head tracking is bounded by**: `headflow`'s
own documentation says 1:1 locking wants under 20 ms, and choosing which screen
somebody faces is comfortable at 107 ms. It bins columns four wide, so 640
across is already 160 bins.

`Fastest` now maximises the frame rate at or above `MinTrackingWidth`, and the
pick is **25 fps instead of 5**.

⚠ **MJPEG is faster on this camera and is still not the default** — but the
reason changed. It used to be that there was no JPEG decode in the frame path.
There is now: `image/jpeg` is in the standard library and is pure Go, so it
needed no cgo and no exception to this module's `CGO_ENABLED=0` — the decoder was
in the toolchain all along.

What MJPEG costs is a **decode**, and whether that fits inside a frame interval
depends on the processor that has to fit it. Measured on this workstation:

| size | MJPEG decode | share of a 30 fps interval |
|---|---|---|
| 320×240 | 1.04–1.09 ms | **3 %** |
| 640×480 | 5.0–6.0 ms | **15–18 %** |
| 1920×1080 | 22.8–27.9 ms | **69–84 %** |

(YUY2 is 0.25–0.43 ms at 640×480 for comparison, from the same benchmark on the
same machine, under load average 15.)

⛔ **And those figures are a workstation's: an optimistic bound for the phone,
not a measurement of it.** The phone, measured through
`UVCStreamStats.Decoding`:

| | workstation | **the Pixel** |
|---|---|---|
| YUY2 640×480 | 0.25–0.43 ms | **3.86 ms** |
| MJPEG 640×480 | 5.0–6.0 ms | **19.84 ms** |
| MJPEG 320×240 | 1.04–1.09 ms | **1.0 ms** |

Three and a half to ten times slower at 640×480, so MJPEG there is **60 % of a
30 fps interval** where the Mac said 15–18 %. Keeping `PickFormat` on YUY2 until
that run existed was the right call, and the run confirms it rather than merely
permitting it — MJPEG also delivered 1.97 fps at `StreamURBs` = 4 and 3.21 at
16, for reasons that are **not** settled and are set out below.

### What is written, and what is not

`cmd/xrusb` and the layer under it go from "the kernel accepts the request" all
the way **to pixels on an unrooted phone** — measured, see the stream below,
where the frame rate that remains short of the camera's own is also recorded.
The parts most likely to be silently wrong are pinned:

- the **ioctl numbers are computed** from this package's own mirrors of the
  kernel's structs, and the suite asserts the results against the values
  `usbdevice_fs.h` produces. A mirror that drifted changes the number, and a
  wrong ioctl number is not a compile error and not necessarily even an error;
- the **URB type is not the endpoint attribute**. An endpoint whose
  `bmAttributes` say 1 is isochronous; a URB whose type is 1 is INTERRUPT. The
  mapping is a table rather than a function that can fail, because a transfer
  type is two bits and an error return there would be a branch no input can
  reach — which the coverage gate found;
- **`wMaxPacketSize` is decoded, not passed.** `USBEndpoint.ISOPacketSize`
  multiplies the low eleven bits by the transactions per microframe. The raw
  field cost a day: the kernel answered `EMSGSIZE` and it was published as a
  refusal;
- the descriptors are read with `pread(2)` **on the lent descriptor**, never by
  reopening a path to it.

### The stream: reaping, assembling, decoding

`USBHandle.Stream` keeps `StreamURBs` = 16 isochronous requests in flight, reaps
them without blocking, assembles payloads into frames and decodes YUY2 or MJPEG
into an `*image.RGBA` — which is what `headflow` takes.

⛔ **Sixteen in flight is not a tuning choice, and neither was the 4 it replaced.**
An isochronous endpoint is scheduled every microframe whether anybody is
listening: between reaping one request and submitting the next, the camera's
packets go nowhere. A high-speed isochronous endpoint carries one packet per
**125 µs microframe**, so `StreamPacketsPerURB` = 8 packets is **one
millisecond** of video a request: sixteen in flight cover **16 ms** of schedule,
and the four this shipped with for five releases covered **4 ms**. That was the
frame rate's ceiling — see *The ring was the ceiling* below. Thirty-two, on
either axis, makes this camera stop sending altogether, which is why the next
move was structural rather than another doubling.

⛔ **This paragraph said "each covers four milliseconds" until 2026-10-25, and
that number contradicted the two beside it.** At four, sixteen requests would
cover 64 ms rather than the 16 ms the same sentence claimed — and the 19 ms
MJPEG decode would have fitted *inside* the ring instead of overrunning it,
which is the step the starvation argument below turns on. Two of this
repository's own measurements already said one millisecond: `ISOPacketsPerURB`
("8 packets is a millisecond of video") and the byte budget's **8000 intervals a
second**. The wrong figure was repeated in four places and the conclusions that
rest on it survive unchanged, which is luck rather than rigour: an argument whose
arithmetic does not close is not an argument.

⛔⛔ **The reaping is a goroutine, and the decode is NOT in it.** `Frame` used to
reap, assemble *and* decode in turn, so every millisecond of decoding was a
millisecond with nothing submitted — the starvation above, wearing the
consumer's hat instead of the ring's. The reader now does nothing but reap,
assemble and resubmit, and hands whole frames over a channel of depth
`MaxPendingFrames`; the decode happens on the caller's goroutine, where its cost
is the caller's. The handover keeps the **freshest** frames: when it is full,
room is made at the FRONT and the shed frame is counted in `Late`, because a
tracker wants the newest picture and the oldest is the one worth losing.

⚠ `MaxPendingFrames` is deliberately **not** `StreamURBs`. The two numbers answer
different questions — how many requests are in flight, and how stale a frame may
be before a tracker stops wanting it. They were the same constant until raising
the ring made the shedding branch unreachable, and the coverage gate said so.

The decisions that are easy to get silently wrong, and what each one is:

| | |
|---|---|
| **the frame boundary is the FID toggle, not the end-of-frame bit** | cameras that never set EOF exist, and on one of those a boundary waiting for it would run every frame into the next forever and deliver nothing. EOF is still honoured when it comes, because it ends a frame one payload earlier |
| **`bHeaderLength` is the offset and is NOT constant** | cameras use 2, 12 and more. A fixed two bytes prepends ten bytes of timestamp to every frame, which does not fail — it shears the image by five YUY2 pixels and looks like a lens mounted crooked |
| **a short frame is dropped, never padded** | a lost packet leaves a band missing, and zeros there are a black stripe that a tracker reads as *structure*. Worse than no frame |
| **`Late` is counted apart from `Dropped`** | a dropped frame is the bus's fault and says the link is lossy; a late one is OURS and says the consumer is slower than the camera. One counter for both makes a slow tracker look like a bad cable |
| **luminance goes into all three channels** | not a shortcut: `headflow.Profile` sums R+G+B down each column, so grey carries exactly what it reads, and every other byte of YUY2 *is* the luminance. A full YUV conversion would compute chrominance for it to add up and discard |
| **a reaped URB is matched by ADDRESS, compared as a number** | converting the kernel's answer back to a `*usbdevfs_urb` is the unsafe misuse `go vet` refuses, and nothing keeps that object alive across the round trip. The slot already knows its own memory |

⚠ **The third row is a YUY2 rule and not a universal one.** A compressed frame's
length is whatever the compressor made it, so "shorter than expected" describes a
*scene* rather than a loss; what tells a finished MJPEG frame from one the bus cut
in half is its **markers**. `NewFrameAssemblerFor` takes the format for exactly
that reason — the two ways of deciding "whole" are not interchangeable, and
either one applied to the other format produces nothing.

⛔⛔ **One request can complete TWO frames, and the first version of this code
returned only the last.** A frame small enough to fit in fewer than eight
packets finishes twice inside one `drain`, so `Stats` said two frames and the
caller got one — and the next call then hung until its deadline. It does not
happen at 640×480, where a frame is some two hundred packets, which is exactly
why a test found it and reading the code would not have. Completed frames are
queued now, and the queue has a ceiling that sheds the **oldest**, because head
tracking wants the freshest.

Two things the device chooses rather than this process, and so are bounded:

- **the memory the kernel writes into is held by the DESCRIPTOR, not by the
  stream.** A submitted request has the kernel writing into this process's heap
  on the camera's schedule until that request is discarded or the fd closes. A
  `UVCStream` that became unreachable without `Close` would let its slots be
  collected while the kernel still held their addresses: a use-after-free
  performed by the kernel, landing in whatever the allocator handed out next,
  and reported by nothing. The handle pins them, tying that memory to the one
  thing whose closing actually stops the writes;
- **a frame's size comes from two 16-bit descriptor fields**, so a broken or
  hostile camera can ask for 65535×65535 — 8.6 GB, allocated twice over before
  anything else happens. `MaxFrameBytes` refuses it, with the check written as a
  division because the product it guards against is the one that overflows.

### ⭐⭐ The pixels arrive — measured on the device, 2026-10-11

```
STEP 7b alternate 5          OK — 2400 bytes an interval for a 2400-byte payload
STEP 8 stream                10 frame(s) in 1.061s
    10 frames, 12 dropped, 0 late, 0 bad payloads; 8232 packets (0 empty), 6846160 bytes
ANSWER 10 WHOLE FRAMES at 9.42 fps, luminance 7..57
```

Pixel 11 Pro Fold, VITURE Beast, **no root**. The **luminance spread** is
reported rather than the frame count because a black frame looks exactly like a
working stream to a counter — four runs gave 7..58, 6..57, 7..56 and 7..57, so
there is a picture and it is stable.

Three defects stood between the agreement and the picture, and each one took the
measurement to find:

⛔⛔ **A cancelled request still completes, and still waits in the queue.** The
first run reported `0 frame(s) in 0s — 0 packets` and the verdict read that as a
silent camera. It was not: step 4's own `SubmitISO` discards its test request
without collecting it, the completion queue belongs to the **descriptor** rather
than to whoever submitted, and the stream picked that orphan up on its first
reap:

```
STOPPED BY: … completed a request at 0x79f466a000 that this stream does not own
```

A question has to clear away its own litter, which `reapOwn` now does. And the
stream *skips and counts* a foreign completion instead of dying on it, because
one stray request from anything else reading the same device must not end a
working camera.

⛔⛔ **The alternate setting goes AFTER the commit, not before.** With it selected
first the camera sent nothing for fifteen seconds while the bus cheerfully
delivered **119 464 empty packets** — one request a millisecond, exactly the
isochronous schedule, and no data at all. UVC takes the move *away* from the
zero-bandwidth alternate, following a commit, as the signal to start sending;
arriving there beforehand means that signal never happens. It is the order the
kernel's own `uvcvideo` uses. `PickAlternate` then takes the **smallest
alternate that fits** the committed payload — alternate 5, 2400 bytes for a
2400-byte payload, where the old code reserved 3072 bytes of every microframe to
carry the same thing.

⛔ **And a `break` that dropped its error cost a cycle on its own.** `0 packets
in 0s` is not a quiet camera: a quiet one still has its endpoint scheduled and
still fills the wait. The reason was sitting in a discarded variable. The probe
prints it now, and switches on the errno.

### ⭐⭐ "The frame rate is ~10 and not 25, and that is the CAMERA" — measured three ways, and WRONG

⛔ The heading is kept in quotation marks because it is what this section
originally claimed, and a heading is what a skimming reader takes away. It was
**us**, not the camera; the refutation is in this section and in the next one.

Three runs on 2026-10-18, same phone, same session, after the phantom frames
below were fixed:

| | YUY2 640×480 | YUY2 320×240 | MJPEG 640×480 |
|---|---|---|---|
| delivered | **9.41 fps** | **10 fps** | **1.97 fps** (3.21 at `StreamURBs` = 16) |
| dropped | 1 | **0** | 67, every one missing its tail (68 at 16) |
| payload the camera committed to | 2400 B | 800 B | 800 B |
| decode, **on the phone** | 3.86 ms | 1.0 ms | **19.84 ms** |

⛔⛔ **AND THE CONCLUSION DRAWN FROM THAT TABLE WAS WRONG.** It said: *flat at
~10 fps across a fourfold change in pixel count, so a ceiling on the camera's own
frame production.* Every number above was taken with `StreamURBs = 4`, and the
ring was starving — the consumer worked for longer than four milliseconds and the
packets arriving in the gap were gone. Raising it to **16**, on the same phone in
the same session:

| `StreamURBs` | fps | dropped |
|---|---|---|
| 4 | 10.5 | 79 |
| 8 | 21.5 | 14 |
| **16** | **23.8** | **0** |
| 32 | 0 | the camera stops sending |

**23.8 fps against the 25 the camera advertises, and nothing dropped.** The
camera was never the ceiling; we were. At 320×240 the same change takes 10 fps
and 10 drops to **14.8 fps and none** — measured over 10 frames, which includes
the start-up wait, so its *sustained* rate is **not** measured and the "flat
across pixel count" claim is withdrawn rather than replaced.

### ⛔⛔⛔ 23.8 fps DID NOT REPRODUCE on 2026-10-31, and the sweep above still holds

Four runs on the same phone, the same glasses and the same commit, in the
configuration that produced the 23.8 above — default pick, `YUY2 640×480 @25fps`,
payload **2400**, alternate **5**, `StreamURBs = 16`:

| | fps | dropped | late |
|---|---|---|---|
| forced `-width 640` | 9.97 | 0 | 0 |
| repeat | 10.0 | 0 | 0 |
| facing a bright light | 9.98 | 0 | 0 |
| default pick (what the sweep used) | 10.0 | 0 | 0 |

And the ring sweep, repeated the same evening: **4 → 8.91 fps and 2 dropped;
16 → 10.0 and none.** So the *shape* of the sweep is reproducible — a deeper
ring still removes the drops — while the *absolute* rate is less than half.

⛔ **What differs is the camera's own byte rate, for an identical negotiation.**
6 239 904 bytes in 7 992 packets is **781 bytes an interval** of the 2400 it
reserved, and the total is exactly ten 614 400-byte frames plus 7 992 × 12 bytes
of UVC header — so nothing is missing and nothing is ours: the camera emitted
ten frames in a second. The afternoon of 18 October it must have emitted
~1 830 bytes an interval to deliver 23.8.

⚠ **It is NOT the light**, which was the obvious guess and is refuted: a run at
luminance `4..86` and one at `37..255` returned **the same byte and packet
totals** (6 239 904 / 7 992). YUY2 frames are a fixed size, so its byte rate
*is* its frame rate — and that rate did not move when the scene brightened by a
factor of three.

**So 23.8 fps is a figure of that evening's camera state, not a property of this
device, and the sentence that presented it as "the" rate was wrong.** What
`StreamURBs = 16` is justified by survives: it removes the dropped frames in
both regimes. What is unexplained is why the same camera, identically
negotiated, gives 1 830 bytes an interval one day and 781 another — the glasses
were being **worn** for the 18 October runs and were on a desk for these, which
is the next variable to try and was not controlled either time.

⚠ **The experiment that misled me was right about its own conditions.** "Sixteen
requests changed nothing" was measured with a consumer costing 0.3 ms, where a
four-millisecond ring is ample. Ring depth matters exactly when the consumer
costs more than the ring covers — which is the mechanism I had already measured
as *too small to matter* and noted was "real and simply below threshold". It went
above threshold the moment a tracker joined the loop, and I did not re-run the
experiment under the new condition. **A negative result carries its conditions
with it.**

⚠ **MJPEG is slower on this camera, and WHY is an open question.** It advertises
30 fps at every size and negotiates a **third** of the bandwidth (800 bytes an
interval against YUY2's 2400); it delivered **1.97 fps** at `StreamURBs` = 4 and
**3.21 fps** at 16, with every loss showing a start-of-image and no
end-of-image — ten whole JPEGs out of 77 boundaries in the first run, 68 losses
of the same shape in the second.

⛔⛔ **I published that as "the camera truncates its own frames", and that
conclusion is WITHDRAWN.** A missing tail is where the bytes stop arriving; it
does not say who stopped sending them. The same signature is exactly what
starvation produces — and starvation was demonstrably still present in the
second run, because the MJPEG decode measured **19.09 ms** against a ring
covering 16 ms, so the ring was empty for part of every frame. The honest
statement is that the losses are **located** at the end of the picture and the
cause is **not established**; mistaking a location for a cause is the same error
this section already records twice.

The decode has since moved off the reaping path entirely (see *The stream*
above), and two runs were taken on **2026-10-31**, twenty minutes apart, same
commit, same glasses, same forced `MJPEG 640×480` (payload **800**, alternate 3):

| | fps | dropped | late | packets | packets/s |
|---|---|---|---|---|---|
| first | **3.44** | 32 | **0** | 23 272 in 2.91 s | ≈8 000 |
| second | **1.71** | 80 | **0** | 46 824 in 5.85 s | ≈8 001 |

**Every loss in both runs lacked its end-of-image and none lacked a start**
(32 and 80 respectively): the camera opened 90 pictures and finished 10.

⭐⭐ **Three numbers exclude OUR starvation, without comparing the runs to
anything.** `0 late` in both says the handover never shed a frame, so the caller
was never behind. ≈8 000 packets a second is **one per microframe** — the
isochronous schedule was *fully claimed*, so nothing could be lost for want of a
request in flight. And 3 288 392 bytes over 46 824 packets is **70 bytes an
interval of the 800 the camera reserved**: it is not that we failed to collect
what it sent, it is that it sent almost nothing. Whatever drops the tails is on
the far side of the bus.

⚠ **No rate comparison is offered, and the earlier one is withdrawn.** "3.21 →
3.44, so the decoupling bought nothing" was a delta across two evenings — and
these two runs, twenty minutes apart, differ by a **factor of two** on their
own. On this camera a rate delta measures the camera's state, not the code; only
the within-run invariants above are worth anything. The YUY2 control of the same
night (above) shows the same instability from the other side.

⛔⛔ **And it cannot be settled in a test here either, which was tried on
2026-10-25.** The suite's fake bus can now lose the schedule nobody covered — a
request covers eight microframes, and whatever arrives in an uncovered gap is
discarded and counted — so the obvious move was to assert that a caller slower
than the whole ring never causes a loss. That assertion was written, and then
checked the only way worth checking: by **breaking the reader** and seeing
whether it noticed.

| the reader was broken by | the test failed by |
| --- | --- |
| never resubmitting at all | **timeout** |
| sleeping 20 ms between the reap and the resubmit — the old coupling exactly | **timeout** |

Both mutations were caught and **neither was caught by the assertion**. The loss
counter never fired, for a structural reason: in that fake a request sits
outstanding until somebody reaps it, while a real request **completes on the
schedule** and stops collecting whether anybody comes for it or not. That
completion *is* the mechanism — sixteen requests cover 16 ms, the decode took
19, and the last three milliseconds had nothing outstanding. A fake with no
notion of completion cannot tell the old coupling from the new reader.

So the test is **not** in the suite. An assertion whose premise cannot hold
passes for the wrong reason, and this one would have read as proof that v0.17.0
fixed the starvation. Earning that proof needs a **time-driven** fake: slots
that complete on a clock and packets generated on the schedule rather than
queued in advance. The lossy bus and its planted positive are in the suite as
the first half of it.

⭐ **The decode figures are now the phone's**, and they are 3.5–10× the
workstation's: 19.84 ms for MJPEG is **60 % of a 30 fps interval** where the Mac
said 15–18 %. Keeping `PickFormat` on YUY2 until this run existed was the right
call, and the run confirms it rather than merely permitting it.

⚠ **What the rate is enough for** is the question that matters, and `headflow`
answers it: choosing which screen somebody faces is comfortable at 107 ms, and
**23.8 fps is 42**. Holding a picture locked to the head at 1:1 wants under
20 ms, so a frame interval of 42 ms plus 4.8 ms of work is still short of it —
but it is twice as close as the shipped default was, and the gap is now the
camera's 25 fps rather than anything in this code.

### ⭐⭐⭐ And a real head turn, measured end to end

2026-10-18, the Beast worn and plugged into the phone, a left-right sweep. 30
frames captured with `xrusb -keep 30`, pulled back with `xrrun -pull`, replayed
through `headflow` by `go-xrkit/xrkit/cmd/headflowreplay`:

| frames | yaw |
|---|---|
| 2 → 9 | 0° → **−52°** |
| 9 → 17 | back through **0.000°** |
| 17 → 25 | 0° → **+80°** |
| 25 → 30 | the return begins, +65.7° |

**197.9° of travel in 2.9 s**, 27 of 30 frames tracked, one refused at
confidence 0.257 — which reacquires, and loses the motion during the gap, exactly
as that behaviour is documented.

⭐⭐ **What convinces here is the SHAPE, not the number.** Out, back *through
zero*, out the other way: that is the gesture. No reference instrument existed to
compare an angle against, so the shape of the movement was the only oracle
available — and a tracker that was merely noisy, or drifting, could not produce
it.

⚠ Confidence runs 0.30–0.94 and **dips at the ends of the sweep**, where the head
moves fastest: motion blur, at 100 ms a frame. And the frames are **pictures of a
room and of whoever is wearing the headset** — they went to the durable
directory outside every work tree that `go-appdirs/outdir` chooses, and were
deleted from the phone and the workstation once the measurement existed. That is
the whole of the hygiene, and `xrrun` says so when it brings them back.

### ⭐⭐⭐ And live, on the phone: 0.9 % of a frame interval

`cmd/xrhead` streams and tracks in **one process on the device**, which the
replay could not speak to: there the tracker ran on a workstation, against files,
with no clock.

```
TRACKED 91 frame(s) in 8.001s — 11.4 fps
    86 used, 3 reference(s), 2 refused for want of confidence
    the tracker itself: 72ms in all, 790µs a frame
    the decode: 4.04ms a frame
    consecutive frames: 90 differed, 0 identical
ANSWER 748.5° of travel, -26.3° to 113.2°, over 86 matched frame(s)
```

**790 µs of an 88 ms interval is 0.9 %**, and the decode is another 4.6 %. The
arithmetic is free — which was a prediction until this ran, and predictions have
been wrong twice in this file.

⛔⛔ **AND THAT RUN'S 11.4 fps WAS OUR OWN CEILING, WHICH THIS PROBE IS WHAT
EXPOSED.** It drops 101 frames for every 91 delivered, and a consumer costing
4.8 ms against a ring covering 4 ms is exactly the starvation this file had
declared too small to matter. Raising `StreamURBs` to 16 gives **23.8 fps and
nothing dropped**; see [the ring](#-the-ring-depth-was-the-cause-and-the-control-that-said-otherwise-was-narrow).
The numbers above are kept as they were taken, because the probe that found the
ceiling is the one that was running under it.

⛔⛔ **The row that says `90 differed, 0 identical` is there because two earlier
runs read "the head did not move" and I could not tell whether that was true.**
0° of travel with 89 frames matched at good confidence is consistent with a still
head *and* with wiring that is blind — and the stream hands back a **reused**
image, which is exactly the shape of a bug that would produce a confident zero.
Comparing one row of the middle band between frames tells them apart, costs a few
hundred byte comparisons against an 88 ms interval, and turned two unreadable
runs into a measured negative: the scene really was static. An instrument that
cannot fail is not an instrument.

⛔⛔ **And the dependency this needed cost ONE LINE, against an argument this file
made four times.** "`go-xrkit/xrkit` pulls in HCL and eight indirect modules, and
a USB layer has no business acquiring a configuration language" — measured,
importing `headflow` adds **one** line to `go.mod` and **two** to `go.sum`,
because Go prunes a dependency's requirements for packages you do not import and
`headflow` imports only the standard library. The file interface that argument
justified is still worth having — offline replay, and pictures a person can look
at — but it was justified by a reason that was not true, and asserting a cost
four times without measuring it is the thing to notice.

#### The per-boundary census, and the diagnosis it first produced

Before the fix below, the same run read:

```
boundaries: 11 toggle(s), 11 EOF bit(s); the dropped frames were short by
            564118 bytes each on average of a 614400-byte frame
the losses ended on: 11 toggle(s), 1 EOF bit(s)
```

⛔⛔ **AND THAT READING WAS WRONG — THE BUG WAS MINE.** Every one of those
"dropped frames" was invented here. A **header-only payload** — a length, the
bits, and nothing after them — is how a camera keeps an isochronous stream alive
between frames. It is *not* an empty packet, so the zero-length skip never saw
it, and it parses perfectly: opening a frame on it meant the next identifier
toggle completed a buffer with nothing in it, once per real frame. Measured at
320×240 on 2026-10-18, one condition changed:

| | frames | dropped | toggles |
|---|---|---|---|
| before | 10 | **10** | 10 |
| after | 10 | **0** | **0** |

At 640×480 the same change took the dropped count from **12 to 1**, and the
toggle count to **zero at both sizes**. *Short by exactly a whole frame* is a
frame that carried nothing — the counters said the bus was losing half the
frames while the bus was losing none. The diagnosis this section used to carry,
that the camera sent same-identifier packets after its end-of-frame bit, was
this defect reading itself back.

⚠ A header-only payload **does** still end a frame that is already open: that is
the legitimate way to close one whose data has all arrived, and ignoring it would
hold every frame until the next toggle — a whole frame-time of latency added to
the one method latency bounds.

#### ⭐⭐ And the byte budget says why: the camera under-fills every interval

Worked through from the numbers of that same run, and it **closes**:

| | |
|---|---|
| packets a second | **7759** of 8000 high-speed intervals — **97 % of the schedule** |
| bytes in each | **832** of the **2400** it committed to — **34.7 % full** |
| so, delivered | **6.45 MB/s, which is 10.5 frames a second of data** |
| needed for 25 fps | **1920** bytes an interval — *under* the allowance |
| 10 whole frames + 12 fragments | 6 144 000 + 702 160 = **6 846 160 bytes, the measured total** |

So **no packets are being lost at all** — 97 % of the schedule arrives — and the
frame rate is bounded by *how much the camera puts in each packet*.

⛔⛔ **THE BUDGET CLOSED AND THE CAUSAL READING WAS BACKWARDS.** Every figure in
that table is real, and "the camera under-fills each interval" is a true
description of what the bytes did. What it is not is a statement about the
camera: with `StreamURBs = 16` the *same camera* fills **77 %** of each interval
and delivers 23.8 fps. It under-filled because **nothing was asking** — the ring
was dry while the consumer worked, and an isochronous endpoint with no request
outstanding is a schedule with nowhere to put the bytes.

⭐ **An identity that balances tells you which quantities are related, not which
of them the device is holding fixed.** I read "832 bytes an interval" as the
camera's choice; it was ours. The way to tell is to vary one of them on the
device, which the sweep above finally did.

⛔ **Two of my own explanations died here, and one of them came back.** That the
ring was too shallow: *refuted* by sixteen requests delivering the same ten
frames — and **that refutation was wrong**, because it was measured with a
consumer costing 0.3 ms where four milliseconds of ring is ample. That the
*consumer* starved it: refuted by a benchmark —

```
BenchmarkDecodeAFrame/640x480     0.25–0.43 ms/frame   0 allocs
BenchmarkAssembleAFrame            0.08–0.15 ms/frame   1 alloc
```

⚠ taken under load average 121, which this fleet has a rule against, and the
numbers spread accordingly. The conclusion survives it because it is an
order-of-magnitude question rather than a close one: a hole of 32 ms cannot be
explained by 0.3 ms of work.

⛔⛔ **AND THAT WAS THE RIGHT ANSWER TO THE WRONG QUESTION.** `cmd/xrusb`'s
consumer costs 0.3 ms, so for *it* the ring is never starved — and I generalised
that into "ring depth is not what bounds this", which held until a **tracker**
joined the loop and the consumer cost 4.8 ms. Then the ring at 4 lost two frames
in three. The benchmark measured the consumer I had; the conclusion was about
every consumer there would ever be.

**The prediction this earned**: a smaller frame should reach the full rate.
320×240 at 25 fps needs **480 bytes an interval**, well inside the 832 measured,
and `MinTrackingWidth` is 320 so `headflow` still accepts it — four-wide bins
make eighty of them.

⛔⛔ **IT WAS WRONG, AND THE DEVICE SAID SO — TWICE, IN OPPOSITE DIRECTIONS.**
320×240 delivered **10 fps**, the same as 640×480, and I read that as the camera
choosing both its byte rate and its frame rate. It was the **ring** choosing
both: with `StreamURBs = 16` the same 320×240 gives **14.8 fps and no drops**,
and 640×480 gives **23.8**. The prediction was wrong, the refutation of it was
wrong, and what settled it was varying the one thing neither had varied.

⭐ **And MJPEG is in the frame path now**, which was the other half of the
prediction, and it is **slower on this camera** — 1.97 fps at a ring of 4, 3.21
at 16, for reasons not yet established. A compressed frame is
whole by its **markers** and an uncompressed one by its **length**, so the
assembler had to learn which it is holding: `NewFrameAssemblerFor` takes the
format, and `UVCNegotiation.Kind` is where it comes from, because a negotiation
result should say what was agreed rather than leave a stream to guess.

⚠ **What a closed byte budget does and does not buy.** It refuted two
explanations that were wrong, which was worth the arithmetic. It then produced a
prediction that was *also* wrong — because an identity that balances tells you
which quantities are related, not which of them the device is holding fixed. Only
varying one of them on the device says that.

### ⛔⛔ The ring depth WAS the cause, and the control that said otherwise was narrow

Deepening it looked obviously right: a 640×480 YUY2 frame at 2400 bytes an
interval takes 32 ms, while four 8-packet requests cover four. The first
measurement, with `cmd/xrusb` and its 0.3 ms consumer:

| requests × packets | packets/s | whole frames |
|---|---|---|
| 4 × 8 | 7744 | 10 |
| 16 × 8 | 7968 | 10 |
| 4 × 32 | **480** | **0** |

⛔ Read as "ring depth is not what bounds this", which **held only for that
consumer**. With `cmd/xrhead` spending 4.8 ms a frame decoding and tracking, the
same axis gives 10.5 → 21.5 → **23.8 fps** and 79 → 14 → **0** dropped. The
shipped default of 4 was halving the frame rate for five releases.

⚠ Thirty-two *requests* fails the same way thirty-two *packets a request* does —
and neither is explained by their product, since 4 × 32 and 16 × 8 are both 128
packets and only one of them works. Two axes, two ceilings, measured not reasoned.

Thirty-two packets a request made the camera stop sending altogether — 480
packets in fifteen seconds, carrying eleven bytes each. Putting 8 back with the
device in the same state streamed again immediately, which is what makes the
ring the cause of that collapse and not the phone's leftover state.

Both lanes are at 100 % statement coverage and race-clean, and a fake camera
drives the whole path from packets to pixels off the device.

⛔ That lock screen cost three measurement cycles, every one of them at the end
of a build. It is now read **before** anything is built — `tools/xrrun` asks
`dumpsys` and names which of the two conditions is in the way, screen or
keyguard — and the host catches the exception instead of dying in `onResume`.

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

## ⛔ When the workstation cannot reach the phone, suspect the WORKSTATION

Every command here runs over a cable, and the phone has **one** USB-C port that
the glasses want all of. The obvious escape is `adb` over Wi-Fi. When that fails
the symptom is "I cannot reach my phone", and an evening went into a diagnosis
that was wrong three times before it was right. The ending is worth more than
the measurements, so it comes first.

### What it turned out to be

A **leftover macOS network extension** — an uninstalled VPN client whose system
extension was still registered and still intercepting traffic:

```
$ systemextensionsctl list
enabled active teamID     bundleID (version)                             [state]
        *      47R2M6779T dev.firezone.firezone.network-extension (1.5.8) [activated waiting for user]
```

Removing it in **System Settings → General → Login Items & Extensions → Network
Extensions** fixed everything at once: `ping`, `adb connect`, and `adb mdns
services`, which had been returning an empty list while macOS's own `dns-sd`
found the phone perfectly.

### The recipe, once nothing is in the way

Wireless debugging, on the phone's Settings → System → Developer options. The
pairing port and the connect port are DIFFERENT, and both are on that screen:

```sh
adb pair 192.168.2.5:35325      # the pairing port, then type the 6-digit code
adb connect 192.168.2.5:46187   # the listening port, from the same screen
```

From then on `adb mdns services` lists the device by name and `adb -s
adb-<serial>-<suffix>._adb-tls-connect._tcp shell …` reaches it with no address
at all. While the extension was in the way, `adb pair` answered `protocol fault
(couldn't read status message)` and `adb mdns services` came back EMPTY — both
of which read as adb being broken rather than as something eating its traffic.

### ⛔ It was named as a suspect and then EXONERATED on bad reasoning

Three pieces of circumstantial evidence said it was inert, and all three were
beside the point:

| the argument | why it was worthless |
|---|---|
| no application in `/Applications` | an extension is registered with the system, not with the app. Deleting the app does not unregister it |
| no process in `ps` | a network extension runs as a system-managed provider, not as something with the vendor's name on it |
| `[activated waiting for user]` | a **state string**. The fact is the `*` in the **active** column, one field to its left, and it said the extension was active |

The listing had the answer in a column and prose beside it, and the prose was
read. **Before clearing a component, check the field that states its status, not
the one that describes it.**

### ⛔ And the control varied the wrong end

The same failure appeared on **two different access points** — a macOS Internet
Sharing hotspot and a Freebox on 6 GHz — which was taken as a control: vary the
access point, the symptom stays, therefore *the phone* is the constant. The
access point was varied. The phone was varied, by having a third machine ping it
successfully. **The workstation was never varied**, and the workstation was the
answer.

A control names what you changed. It cannot name what you did not think to
change, and "everything else" is where the cause was sitting the whole time.

### ⭐ The ten-second test that should have come first

```
Mac     → phone   0/3
fractal → phone   2/2, 142–294 ms       ← a third machine, straight away
```

One line from a third machine splits the problem in half: either the phone is
refusing everyone, or it is one path that is broken. It costs nothing, needs no
theory, and it is the first thing to do rather than the last.

### ⚠ A failure that is too fast never left the machine

Attempts failed in **5 ms** with `EHOSTUNREACH`, read here as an active ICMP
rejection. It is not: it is **macOS's negative ARP cache** short-circuiting the
attempt without putting a packet on the wire. After a reboot cleared it, the
same ping took its full **4.5 seconds** — a silent drop, a different mechanism
with different causes. Read the duration before the error: a reachable host with
a closed port takes about a second, and an instant failure is local state.

### Local Network Protection: real, enabled, and NOT the cause

It was blamed here, at length, and it was wrong. From **Android 17** the
protection is mandatory and traffic to and from a local network address needs
`ACCESS_LOCAL_NETWORK`, *including accepting incoming TCP connections* — see
[the documentation](https://developer.android.com/privacy-and-security/local-network-permission).
On the device:

```
$ adb shell device_config get android_core_networking \
      android.permission.flags.access_local_network_permission_enabled
true
```

That is true, and it matters for **any application in this repository that wants
to be reachable**: it must declare and request that permission, and nothing here
does yet. It did not stop `adb`, which is a system daemon, and it never had
anything to do with `ping`. The flag cannot be turned off from a shell in any
case — `device_config put` answers *"allowlist permission granted, but must add
flag to the allowlist"* on a release build.

Android 17 also ships **ADB Wi-Fi 2.0**, a rewritten wireless-debugging stack
needing `adb` 37 or newer on the host. Wireless debugging works **better** on
Android 17, not worse, and that fact was in hand early enough to have cast doubt
on the whole diagnosis.

### What this repository does regardless

Each command writes its verdict with `android.SaveTranscript` into the app's
private directory, which survives the cable being somewhere else, and
`adb shell run-as <pkg> cat files/<name>.txt` collects it afterwards. That was
built for the one-port problem, it is unaffected by any of this, and every
measurement in this README was taken that way.

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

## Running a probe on a device

```sh
go run ./tools/xrrun -app ./cmd/xrusb -args '-wait 30s'
```

That is one command for what used to be seven, and it is in Go because **the
mistakes were in the incantation rather than in the code under test**: a
transcript read before it was written and reported as "the probe said nothing";
a stale APK measured because the install was skipped; an argument string the
shell split so that `am` read `5m` as its target package and started nothing.
Each cost a cycle and one of them cost a wrong conclusion.

It waits for the **transcript file** rather than for a duration, notices a
**crash** instead of timing out on one, refuses to choose when two devices are
listed — wireless `adb` lists the same phone twice — and finds the SDK and JDK
itself rather than demanding two exports.

⛔⛔⛔ **And it knows which permissions a probe cannot work without, because the
flag that used to carry them cost two weeks.** That command used to end in
`-perms android.permission.CAMERA`, and leaving it off did not fail where you
could see it: forty seconds later the run reported

```
the permission broadcast for /dev/bus/usb/001/007 came back not granted
```

one millisecond after asking — which reads as *the user refusing*, and sent four
sessions looking at activity lifecycles, pending intents and foreground state.
The cause is that `UsbUserPermissionManager.hasPermission` **short-circuits on
CAMERA** for any device whose `getHasVideoCapture()` is true, before it consults
the USB grant at all, so no USB dialog can fix it and the platform shows none.

So `xrrun` carries a table — `xrusb`, `xrhead` and `xrcam` need `CAMERA` — and
declares *and* grants it, printing the line so a permission is never handed out
invisibly:

```
==> xrusb needs android.permission.CAMERA; declaring and granting it
```

A test asserts both halves: that every camera probe is in the table, and that
`xrscreen` and `xrglasses` are **not** — a harness that hands out permissions
nobody needs is a bad habit on a real person's phone. `-perms` still works and
is merged with the table, once, so the manifest and the grants are given the
same list.

⛔ **And it asks whether the phone can run anything at all, before it builds.**
Behind the lock screen Android does not credit the application as foreground,
however resumed its Activity is, so the host's own `startService` throws and the
process dies in `onResume` — three times, each at the end of a forty-second
build. It reads `dumpsys power` *and* `dumpsys window policy`, because the
wakefulness is in one and the keyguard in the other, and it names which
condition is in the way:

```
… is not usable: it is locked.
⛔ WAKE AND UNLOCK THE PHONE, AND LEAVE IT ON.
  adb -s … shell svc power stayon true   # EVERY power source: this rig reports AC, not USB
  adb -s … shell settings get system screen_off_timeout   # 30000 here

==> waiting up to 3m0s for the phone, asking every second
==> the phone is awake and unlocked; carrying on
```

⛔⛔ **It WAITS for the unlock rather than refusing it, and `-hands` is how
long.** Unlocking is a fingerprint or a code: nothing here can do it and nothing
should. A guard that refuses the instant it is called makes a person time an
unlock to a launch, and every mistimed attempt costs a build and an install —
**five round trips for one unlock**, measured on 2026-10-18. The reason is
printed once, then the phone is asked again every second. `-hands` is separate
from `-patience` because one waits for a machine and the other for somebody to
come back to their desk.

⚠ **The strings it looks for were read off the phone, not taken from the crash
transcript.** The first version grepped `window policy` for `mWakefulness=Dozing`
— a string that section never prints — so it would have passed a sleeping
device, behind ten green hand-written expectations. `isKeyguardShowing` appears
in neither section; the keyguard says `showing=true` and `inputRestricted=true`
in its own delegate block. And `screenState` is **`SCREEN_STATE_ON` while the
phone sleeps**, because of the always-on display, so a check on the screen is
the wrong check. One real transcript is pinned as the fixture.

⛔⛔⛔ **And those strings were still wrong, because this phone FOLDS.** A
foldable has two power groups, one per screen, and `dumpsys power` prints
`mIsInteractive` for each — so on an **awake and unlocked** phone one of them
says `false`, and a `strings.Contains` for it declared the *phone* asleep. The
guard refused three times in a row and sent a person to wake a device that was
already awake, which is the exact round trip it exists to prevent:

```
mWakefulness=Awake                         ← the phone
  Wakefulness Session Power Group powerGroupId: 0
    mIsInteractive: false                  ← one screen, dark
  Wakefulness Session Power Group powerGroupId: 1
    mIsInteractive: true                   ← the other, in use
```

The device-wide `mWakefulness=<word>` decides now, and the per-screen signs only
when that line is missing — and then only if nothing contradicts them, because
one screen awake is all the platform needs. It insists on a **word** because the
same key reappears **numbered** once per power group in a statistics block.

⭐ **The signs HAD been measured on this phone — wholly asleep, where both
screens agree.** A measurement taken in a state where two cases coincide cannot
distinguish them. The fix therefore ships with a control: a phone that really is
`Dozing` while one screen still reports interactive must *still* be refused, or
the guard has merely stopped looking.

⚠ And the command it suggested could not work here. `svc power stayon usb` sets
one bit of `stay_on_while_plugged_in`, and this rig reports `AC powered: true,
USB powered: false` — the phone is charged through the same hub the glasses hang
off. The setting read back as **applied** while the screen kept going dark on a
30-second timeout. **A suggestion that cannot work on the rig it is printed for
is worse than none: it reads as a fix that was tried.** The message is built by
its own function now, so a test can read what a person is told; being reachable
only with a phone attached is how `usb` survived four releases.

⚠ **A section it cannot read is not a refusal**: `dumpsys` gets renamed between
releases, and a tool that refused to run because it could not find a field would
be worse than the crash it avoids — the crash at least names its own cause.

`host/build.sh` stays a script,
because `aapt2`, `javac`, `d8`, `zipalign` and `apksigner` in a row have no
decisions in them.

⚠ **`cmd/xrusb` leaves a debt.** Claiming the camera's streaming interface
detaches `uvcvideo`, and `USBDEVFS_CONNECT` does **not** put it back on this
phone — the ioctl succeeds and the driver still does not rebind:

```
before:  1-1.1:1.0 driver=uvcvideo   1-1.1:1.1 driver=uvcvideo
after:   1-1.1:1.0 driver=uvcvideo   1-1.1:1.1 driver=
```

`/dev/video0` survives on the control interface, but the streaming one is left
with no driver until the headset is **unplugged and plugged back**.
`USBHandle.Detached` reports what was taken, `Close` returns a reconnect it
could not make — it used to swallow it — and the probe says so in its
transcript. Taking something from the whole phone and saying nothing is worse
than failing.

## Testing

`100.0%` statement coverage, gated in CI on **both** lanes — the Linux transport
and the off-Android stub. A stub nobody tests is a stub that panics the day
somebody links it.

⚠ And a portable function whose only test is on one lane is **uncovered on the
other**, which is how `ISOPacketSize` — the arithmetic that cost a day — came to
be tested on Linux alone. The gate caught it from the darwin side.

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
- and the headset's **camera is reachable through usbfs**, on an unrooted phone:
  the descriptor is lent, `uvcvideo` is detached from the streaming interface,
  and the kernel **accepts an isochronous request**. Neither Android camera API
  offers it — no external camera2 device, and the Java USB API cannot submit
  isochronous transfers — so usbfs is the one route. See
  [the census](#following-a-head-the-census-and-the-answer-it-gave) and
  [what came of it](#-usbfs-works-on-an-unrooted-phone).

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
- **no application here is reachable from a workstation**, because none declares
  `ACCESS_LOCAL_NETWORK` — mandatory from Android 17 for anything that accepts
  an incoming connection. The commands sidestep it by writing transcripts, which
  is why they work at all; making one reachable is unwritten;
- **head tracking on Android WORKS, ON THE PHONE, LIVE.** `cmd/xrhead` streams
  and tracks in one process: **748.5° of travel** over 86 matched frames in 8
  seconds, and the tracker itself costs **790 µs of an 88 ms frame interval —
  0.9 %**. A worn
  left-right sweep replayed offline comes back as **197.9° of travel** — out to
  −52°, back through
  zero, out to +80° — through usbfs on an unrooted phone. **The rate is
  23.8 fps with nothing dropped**, against the 25 the camera advertises, once
  `StreamURBs` is 16; at the 4 this shipped with for five releases it was 10.5
  with two frames in three lost, and **an entry here called that the camera's own
  ceiling.** It was ours. MJPEG remains slower on this device (1.97 fps at a ring
  of 4, 3.21 at 16) and **why is an open question**: the losses all lack an
  end-of-image, which locates them without naming a cause, and an earlier entry
  here read that location as "the camera truncates its own frames". That reading
  is withdrawn. Enough for choosing which screen somebody faces,
  which `headflow` puts at 107 ms against this 42; still short of locking a
  picture to the head at 1:1, which wants under 20 ms. **Three earlier entries
  here said this was impossible, a fourth said it was unwritten, and a fifth
  blamed the camera for a rate we were imposing**; the first three were our
  own bugs, the fourth was stale for four releases, and the fifth read our own
  starvation as the hardware's limit. All of them are kept, because being wrong
  in that direction — about what the platform allows, and about whose ceiling a
  number is — is the more useful half;
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
