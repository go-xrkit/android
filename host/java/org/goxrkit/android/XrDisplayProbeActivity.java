// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.goxrkit.android;

import android.app.Activity;
import android.app.Presentation;
import android.content.Context;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Paint;
import android.graphics.PixelFormat;
import android.graphics.drawable.ColorDrawable;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.media.Image;
import android.media.ImageReader;
import android.os.Bundle;
import android.os.Handler;
import android.os.HandlerThread;
import android.util.DisplayMetrics;
import android.util.Log;
import android.view.Display;
import android.view.MotionEvent;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowManager;
import android.widget.TextView;

import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * Asks the platform the ONE question the earlier probe never asked: may an
 * ordinary app show a {@link Presentation} on a virtual display IT created?
 *
 * <p>The earlier work proved two things that sit next to each other and were
 * never confronted. An app-created virtual display comes back carrying
 * {@code flags=0x8 [PRESENTATION]} — literally the flag that says a Presentation
 * may be shown there — and {@code Presentation.show()} was proved to work, but
 * only on the emulator's SYSTEM-created secondary. An activity launch was
 * refused on the app's own display; a Presentation is a Dialog attached to a
 * Display, not an activity start, and the check that refused us was an
 * activity-start check.
 *
 * <p>Every step reports what Android literally answered, and step two asserts on
 * the PIXELS: a Presentation that "succeeds" and delivers a black buffer is the
 * classic silent failure and is the entire point of the experiment.
 */
public final class XrDisplayProbeActivity extends Activity {
    static final String TAG = "xr-probe";

    /** Where a captured frame may be written: the app's external files dir,
     *  which is on the DEVICE and inside no repository. Pulling it to a
     *  workstation is the puller's business, and the test that does it walks the
     *  destination up to the root looking for a .git. */
    private File artifacts;

    private HandlerThread readerThread;
    private Handler readerHandler;
    /** Touch events the PHONE's own view received — question four's evidence. */
    private final AtomicInteger phoneTouches = new AtomicInteger();
    private final AtomicInteger presentationTouches = new AtomicInteger();
    private volatile String lastPhoneTouch = "(none)";

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        artifacts = getExternalFilesDir(null);

        // A plain view on the PHONE's display that counts MotionEvents. It is
        // question four's instrument: the phone as a trackpad while the content
        // people look at is on the other display.
        TextView tv = new TextView(this) {
            @Override
            public boolean onTouchEvent(MotionEvent e) {
                phoneTouches.incrementAndGet();
                lastPhoneTouch = MotionEvent.actionToString(e.getActionMasked())
                        + " (" + e.getX() + "," + e.getY() + ")";
                Log.i(TAG, "TOUCH phone view " + lastPhoneTouch
                        + " displayId=" + getDisplay().getDisplayId());
                return true;
            }
        };
        tv.setText("go-xrkit display probe\n\nadb logcat -s xr-probe");
        tv.setTextSize(18);
        tv.setPadding(48, 200, 48, 48);
        tv.setClickable(true);
        setContentView(tv);

        readerThread = new HandlerThread("xr-probe-reader");
        readerThread.start();
        readerHandler = new Handler(readerThread.getLooper());

        new Thread(this::run, "xr-probe").start();
    }

    private void run() {
        try {
            question5Displays();
            question1and2();
            question3HowMany();
            question4Trackpad();
        } catch (Throwable t) {
            Log.e(TAG, "probe aborted", t);
        }
        Log.i(TAG, "PROBE DONE");
    }

    // ---------------------------------------------------------------- Q5 ----

    /** What every display attached to this device reports, before anything is
     *  created. The "screen larger than the phone" claim is settled here or not
     *  at all. */
    private void question5Displays() {
        DisplayManager dm = getSystemService(DisplayManager.class);
        for (Display d : dm.getDisplays()) {
            Log.i(TAG, "Q5 " + describe(d));
        }
        // "A screen larger than the phone's" is either the glasses' own display
        // used at its own resolution, or it is a virtual one. Settle it by
        // rendering on the display we were GIVEN and reporting the size the
        // window actually got.
        for (Display d : dm.getDisplays()) {
            if (d.getDisplayId() == Display.DEFAULT_DISPLAY) {
                continue;
            }
            Pres p = showPresentation(d, "given");
            if (p == null) {
                continue;
            }
            View v = p.presentation.getWindow().getDecorView();
            Log.i(TAG, "Q5 GIVEN display " + d.getDisplayId() + " \"" + d.getName()
                    + "\": Presentation.show() SUCCEEDED, decor "
                    + v.getWidth() + "x" + v.getHeight()
                    + ", presentation context "
                    + p.presentation.getContext().getResources()
                            .getDisplayMetrics().widthPixels + "x"
                    + p.presentation.getContext().getResources()
                            .getDisplayMetrics().heightPixels
                    + " @" + p.presentation.getContext().getResources()
                            .getDisplayMetrics().densityDpi + "dpi");
            try {
                Thread.sleep(1500);
            } catch (InterruptedException ignored) {
                Thread.currentThread().interrupt();
            }
            Log.i(TAG, "Q5 GIVEN display " + d.getDisplayId() + " after layout, decor "
                    + v.getWidth() + "x" + v.getHeight());
            dismiss(p);
        }
    }

    private String describe(Display d) {
        DisplayMetrics real = new DisplayMetrics();
        d.getRealMetrics(real);
        // "What may an ordinary app render there" has to be asked of a WINDOW
        // context on that display. A plain display context answers with the
        // DEFAULT display's bounds, which is the wrong number and looks right.
        Context wc = createDisplayContext(d).createWindowContext(
                WindowManager.LayoutParams.TYPE_APPLICATION, null);
        android.graphics.Rect bounds = wc.getSystemService(WindowManager.class)
                .getMaximumWindowMetrics().getBounds();
        return "display " + d.getDisplayId() + " \"" + d.getName() + "\""
                + " real=" + real.widthPixels + "x" + real.heightPixels
                + " @" + real.densityDpi + "dpi (density " + real.density + ")"
                + " appBounds=" + bounds.width() + "x" + bounds.height()
                + " refresh=" + String.format("%.1f", d.getRefreshRate()) + "Hz"
                + " flags=0x" + Integer.toHexString(d.getFlags())
                + " state=" + d.getState()
                + " valid=" + d.isValid();
    }

    // ------------------------------------------------------------- Q1+Q2 ----

    private static final int W = 640;
    private static final int H = 480;

    private void question1and2() {
        DisplayManager dm = getSystemService(DisplayManager.class);
        int flags = DisplayManager.VIRTUAL_DISPLAY_FLAG_OWN_CONTENT_ONLY
                | DisplayManager.VIRTUAL_DISPLAY_FLAG_PRESENTATION;

        ImageReader reader = ImageReader.newInstance(W, H, PixelFormat.RGBA_8888, 3);
        VirtualDisplay vd;
        try {
            vd = dm.createVirtualDisplay("xr-probe-own-0", W, H, 320,
                    reader.getSurface(), flags);
        } catch (Throwable t) {
            Log.e(TAG, "Q1 createVirtualDisplay REFUSED: " + t, t);
            reader.close();
            return;
        }
        if (vd == null) {
            Log.e(TAG, "Q1 createVirtualDisplay returned null");
            reader.close();
            return;
        }
        Display d = vd.getDisplay();
        Log.i(TAG, "Q1 created " + describe(d));

        Grab grab = new Grab(reader, "q1");
        Pres p = showPresentation(d, "q1");
        if (p == null) {
            vd.release();
            reader.close();
            return;
        }
        Log.i(TAG, "Q1 ANSWER: Presentation.show() SUCCEEDED on the app's OWN "
                + "virtual display " + d.getDisplayId());

        // Q2: the pixels. A show() that returns and a buffer that arrives are
        // two different facts, and a buffer that arrives BLACK is the third.
        Frame f = grab.await(10, TimeUnit.SECONDS);
        if (f == null) {
            Log.e(TAG, "Q2 ANSWER: NO FRAME arrived in 10s — show() succeeded "
                    + "but the display produced nothing");
        } else {
            Log.i(TAG, "Q2 " + f.report());
            f.assertSentinels("Q2");
            save(f, "presentation-own-virtual-display.png");
        }
        dismiss(p);
        vd.release();
        grab.close();
        reader.close();
    }

    // ---------------------------------------------------------------- Q3 ----

    /** How many at once, and where it stops. */
    private void question3HowMany() {
        // Twice: at a small size, where the answer is about display slots, and
        // at a ribbon-sized one, where it is about graphics memory. A ceiling
        // that moves with the size is a memory limit; one that does not is a
        // system limit, and the two are not the same finding.
        ceiling(640, 480, intExtra("smallCap", 64));
        ceiling(1920, 1080, intExtra("bigCap", 64));
    }

    private int intExtra(String name, int fallback) {
        String v = getIntent().getStringExtra(name);
        if (v == null) {
            return fallback;
        }
        try {
            return Integer.parseInt(v.trim());
        } catch (NumberFormatException e) {
            return fallback;
        }
    }

    /** Creates displays with a Presentation apiece until something refuses, and
     *  reports what refused and how far it got. */
    private void ceiling(int w, int h, int cap) {
        DisplayManager dm = getSystemService(DisplayManager.class);
        int flags = DisplayManager.VIRTUAL_DISPLAY_FLAG_OWN_CONTENT_ONLY
                | DisplayManager.VIRTUAL_DISPLAY_FLAG_PRESENTATION;
        List<VirtualDisplay> vds = new ArrayList<>();
        List<ImageReader> readers = new ArrayList<>();
        List<Pres> pres = new ArrayList<>();
        int good = 0;
        String stopped = "the probe's own cap of " + cap
                + " — the platform had not refused anything";
        for (int i = 0; i < cap; i++) {
            ImageReader r = null;
            VirtualDisplay vd;
            try {
                r = ImageReader.newInstance(w, h, PixelFormat.RGBA_8888, 3);
                vd = dm.createVirtualDisplay("xr-probe-many-" + i, w, h, 320,
                        r.getSurface(), flags);
            } catch (Throwable t) {
                stopped = "createVirtualDisplay #" + i + " threw " + t;
                Log.e(TAG, "Q3 " + w + "x" + h + " stopped: " + stopped, t);
                if (r != null) {
                    r.close();
                }
                break;
            }
            if (vd == null) {
                stopped = "createVirtualDisplay #" + i + " returned null";
                r.close();
                break;
            }
            readers.add(r);
            vds.add(vd);
            Grab g = new Grab(r, "q3-" + i);
            Pres p = showPresentation(vd.getDisplay(), "" + i);
            if (p == null) {
                stopped = "Presentation.show() refused on display #" + i;
                g.close();
                break;
            }
            pres.add(p);
            Frame f = g.await(10, TimeUnit.SECONDS);
            g.close();
            if (f == null) {
                stopped = "display #" + i + " showed but produced NO FRAME in 10s";
                break;
            }
            if (!f.sentinelsHold()) {
                stopped = "display #" + i + " produced a frame that is not what "
                        + "the Presentation drew: " + f.report();
                break;
            }
            good = i + 1;
            if (i == 0 || (i + 1) % 8 == 0) {
                Log.i(TAG, "Q3 " + w + "x" + h + " #" + i + " id="
                        + vd.getDisplay().getDisplayId() + " OK — " + f.report());
            }
        }
        Runtime rt = Runtime.getRuntime();
        Log.i(TAG, "Q3 ANSWER " + w + "x" + h + ": " + good + " simultaneous "
                + "app-created virtual displays, each with a Presentation whose "
                + "pixels arrived CORRECT; stopped by " + stopped
                + "; javaHeap used " + (rt.totalMemory() - rt.freeMemory()) / (1 << 20)
                + "MiB of max " + rt.maxMemory() / (1 << 20) + "MiB");
        for (Pres p : pres) {
            dismiss(p);
        }
        for (VirtualDisplay vd : vds) {
            vd.release();
        }
        for (ImageReader r : readers) {
            r.close();
        }
    }

    // ---------------------------------------------------------------- Q4 ----

    /** The phone's touchscreen while the content is on the OTHER display. */
    private void question4Trackpad() {
        DisplayManager dm = getSystemService(DisplayManager.class);
        int flags = DisplayManager.VIRTUAL_DISPLAY_FLAG_OWN_CONTENT_ONLY
                | DisplayManager.VIRTUAL_DISPLAY_FLAG_PRESENTATION;
        ImageReader reader = ImageReader.newInstance(W, H, PixelFormat.RGBA_8888, 3);
        VirtualDisplay vd = dm.createVirtualDisplay("xr-probe-touch", W, H, 320,
                reader.getSurface(), flags);
        if (vd == null) {
            Log.e(TAG, "Q4 no virtual display");
            reader.close();
            return;
        }
        Pres p = showPresentation(vd.getDisplay(), "q4");
        int before = phoneTouches.get();
        Log.i(TAG, "Q4 READY: presentation up on display " + vd.getDisplay().getDisplayId()
                + ", phone touches so far " + before
                + " — inject taps now (adb shell input tap)");
        // Wait for injected taps rather than for the clock: a probe that sleeps
        // through the window it asked for reports zero and proves nothing.
        long deadline = System.currentTimeMillis() + 40000;
        while (System.currentTimeMillis() < deadline && phoneTouches.get() - before < 6) {
            try {
                Thread.sleep(200);
            } catch (InterruptedException ignored) {
                Thread.currentThread().interrupt();
                break;
            }
        }
        Log.i(TAG, "Q4 ANSWER: phone view received " + (phoneTouches.get() - before)
                + " MotionEvents while its content was on display "
                + vd.getDisplay().getDisplayId()
                + "; the presentation view received " + presentationTouches.get()
                + "; last phone event " + lastPhoneTouch);
        dismiss(p);
        vd.release();
        reader.close();
    }

    // ----------------------------------------------------------- helpers ----

    /** A Presentation plus what happened when it was shown. */
    private static final class Pres {
        Presentation presentation;
        Throwable failure;
    }

    /** Shows a Presentation on {@code d} on the main thread and waits for the
     *  platform's answer, whatever it is. */
    private Pres showPresentation(Display d, String tag) {
        final Pres out = new Pres();
        final CountDownLatch done = new CountDownLatch(1);
        runOnUiThread(() -> {
            try {
                Presentation p = new Presentation(XrDisplayProbeActivity.this, d);
                p.setContentView(new Sentinel(p.getContext(), tag, presentationTouches),
                        new ViewGroup.LayoutParams(
                                ViewGroup.LayoutParams.MATCH_PARENT,
                                ViewGroup.LayoutParams.MATCH_PARENT));
                p.getWindow().setBackgroundDrawable(new ColorDrawable(Color.RED));
                p.getWindow().setLayout(ViewGroup.LayoutParams.MATCH_PARENT,
                        ViewGroup.LayoutParams.MATCH_PARENT);
                p.show();
                out.presentation = p;
            } catch (Throwable t) {
                out.failure = t;
            } finally {
                done.countDown();
            }
        });
        try {
            done.await(10, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
        if (out.failure != null) {
            Log.e(TAG, "Presentation.show() REFUSED on display " + d.getDisplayId()
                    + ": " + out.failure, out.failure);
            return null;
        }
        if (out.presentation == null) {
            Log.e(TAG, "Presentation.show() did not answer in 10s on display "
                    + d.getDisplayId());
            return null;
        }
        return out;
    }

    private void dismiss(Pres p) {
        if (p == null || p.presentation == null) {
            return;
        }
        runOnUiThread(p.presentation::dismiss);
    }

    /**
     * The unmistakable content. Solid quadrants of exactly known colour, so a
     * frame can be checked by SAMPLING rather than by looking: a black buffer,
     * a blank buffer and a buffer carrying somebody else's content all fail.
     */
    private static final class Sentinel extends View {
        static final int BG = 0xFFFF0000;      // red
        static final int TOP_LEFT = 0xFF00FF00;  // green
        static final int BOTTOM_RIGHT = 0xFF0000FF; // blue
        private final Paint paint = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final String label;
        private final AtomicInteger touches;

        Sentinel(Context c, String label, AtomicInteger touches) {
            super(c);
            this.label = label;
            this.touches = touches;
            setClickable(true);
        }

        @Override
        public boolean onTouchEvent(MotionEvent e) {
            touches.incrementAndGet();
            Log.i(TAG, "TOUCH presentation view " + label + " "
                    + MotionEvent.actionToString(e.getActionMasked()));
            return true;
        }

        @Override
        protected void onDraw(Canvas c) {
            int w = getWidth();
            int h = getHeight();
            c.drawColor(BG);
            paint.setColor(TOP_LEFT);
            c.drawRect(0, 0, w / 2f, h / 2f, paint);
            paint.setColor(BOTTOM_RIGHT);
            c.drawRect(w / 2f, h / 2f, w, h, paint);
            paint.setColor(Color.WHITE);
            paint.setTextSize(h / 8f);
            c.drawText(label, w / 2f - h / 8f, h / 2f, paint);
        }
    }

    /** One frame's pixels, already unpacked out of the reader's plane. */
    private final class Frame {
        final int[] argb;
        final int w;
        final int h;
        final int rowStride;
        final int pixelStride;

        Frame(int[] argb, int w, int h, int rowStride, int pixelStride) {
            this.argb = argb;
            this.w = w;
            this.h = h;
            this.rowStride = rowStride;
            this.pixelStride = pixelStride;
        }

        int at(int x, int y) {
            return argb[y * w + x];
        }

        String report() {
            Set<Integer> distinct = new HashSet<>();
            long black = 0;
            long sum = 0;
            for (int p : argb) {
                if (distinct.size() < 64) {
                    distinct.add(p);
                }
                int r = (p >> 16) & 0xFF;
                int g = (p >> 8) & 0xFF;
                int b = p & 0xFF;
                if (r == 0 && g == 0 && b == 0) {
                    black++;
                }
                sum += r + g + b;
            }
            return "frame " + w + "x" + h + " rowStride=" + rowStride
                    + " pixelStride=" + pixelStride
                    + " distinctColours" + (distinct.size() >= 64 ? ">=64" : "=" + distinct.size())
                    + " black=" + black + "/" + (long) w * h
                    + " meanRGB=" + (sum / (3L * w * h))
                    + " topLeft=#" + hex(at(w / 8, h / 8))
                    + " topRight=#" + hex(at(w * 7 / 8, h / 8))
                    + " bottomLeft=#" + hex(at(w / 8, h * 7 / 8))
                    + " bottomRight=#" + hex(at(w * 7 / 8, h * 7 / 8));
        }

        boolean sentinelsHold() {
            return rgb(at(w / 8, h / 8)) == rgb(Sentinel.TOP_LEFT)
                    && rgb(at(w * 7 / 8, h * 7 / 8)) == rgb(Sentinel.BOTTOM_RIGHT)
                    && rgb(at(w * 7 / 8, h / 8)) == rgb(Sentinel.BG)
                    && rgb(at(w / 8, h * 7 / 8)) == rgb(Sentinel.BG);
        }

        void assertSentinels(String q) {
            if (sentinelsHold()) {
                Log.i(TAG, q + " ANSWER: the PIXELS ARRIVED and are the ones the "
                        + "Presentation drew — green top-left, blue bottom-right, "
                        + "red elsewhere, at the exact sampled coordinates");
            } else {
                Log.e(TAG, q + " ANSWER: a frame arrived but it is NOT what the "
                        + "Presentation drew — the silent failure");
            }
        }
    }

    private static int rgb(int argb) {
        return argb & 0xFFFFFF;
    }

    private static String hex(int argb) {
        return String.format("%08X", argb);
    }

    /** Waits for one image out of a reader without ever blocking its thread. */
    private final class Grab {
        private final ImageReader reader;
        private final CountDownLatch latch = new CountDownLatch(1);
        private volatile Frame frame;

        Grab(ImageReader reader, String tag) {
            this.reader = reader;
            reader.setOnImageAvailableListener(r -> {
                Image img = null;
                try {
                    img = r.acquireLatestImage();
                    if (img == null) {
                        return;
                    }
                    if (frame == null) {
                        frame = unpack(img);
                        latch.countDown();
                    }
                } catch (Throwable t) {
                    Log.e(TAG, "grab " + tag + " failed", t);
                } finally {
                    if (img != null) {
                        img.close();
                    }
                }
            }, readerHandler);
        }

        Frame await(long timeout, TimeUnit unit) {
            try {
                latch.await(timeout, unit);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
            return frame;
        }

        void close() {
            reader.setOnImageAvailableListener(null, null);
        }
    }

    private Frame unpack(Image img) {
        Image.Plane plane = img.getPlanes()[0];
        ByteBuffer buf = plane.getBuffer();
        int rowStride = plane.getRowStride();
        int pixelStride = plane.getPixelStride();
        int w = img.getWidth();
        int h = img.getHeight();
        int[] argb = new int[w * h];
        byte[] row = new byte[rowStride];
        for (int y = 0; y < h; y++) {
            buf.position(y * rowStride);
            int n = Math.min(rowStride, buf.remaining());
            buf.get(row, 0, n);
            for (int x = 0; x < w; x++) {
                int o = x * pixelStride;
                int r = row[o] & 0xFF;
                int g = row[o + 1] & 0xFF;
                int b = row[o + 2] & 0xFF;
                int a = row[o + 3] & 0xFF;
                argb[y * w + x] = (a << 24) | (r << 16) | (g << 8) | b;
            }
        }
        return new Frame(argb, w, h, rowStride, pixelStride);
    }

    private void save(Frame f, String name) {
        if (artifacts == null) {
            Log.e(TAG, "no external files directory to write " + name + " into");
            return;
        }
        File out = new File(artifacts, name);
        Bitmap bmp = Bitmap.createBitmap(f.argb, f.w, f.h, Bitmap.Config.ARGB_8888);
        try (FileOutputStream os = new FileOutputStream(out)) {
            bmp.compress(Bitmap.CompressFormat.PNG, 100, os);
            Log.i(TAG, "ARTIFACT " + out.getAbsolutePath());
        } catch (IOException e) {
            Log.e(TAG, "cannot write " + out, e);
        } finally {
            bmp.recycle();
        }
    }
}
