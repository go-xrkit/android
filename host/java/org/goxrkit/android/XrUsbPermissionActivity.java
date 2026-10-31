// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.goxrkit.android;

import android.app.Activity;
import android.app.PendingIntent;
import android.content.Intent;
import android.hardware.usb.UsbDevice;
import android.hardware.usb.UsbManager;
import android.os.Bundle;
import android.util.Log;

/**
 * A transparent Activity whose only job is to ask for permission to open ONE
 * USB device.
 *
 * <h2>⛔ Why a Service cannot ask, measured rather than assumed</h2>
 *
 * {@link UsbManager#requestPermission} called from {@link XrWallService}
 * answered <b>denied in eight milliseconds with no dialog shown</b>. The
 * broadcast was real — 1908 bytes of extras, so the system did reply — and it
 * said no without asking anybody:
 *
 * <pre>
 * 15:44:56.336 asking for permission on /dev/bus/usb/001/011
 * 15:44:56.344 usb permission broadcast: extras=Bundle[dataSize=1908] granted=false
 * </pre>
 *
 * The dialog is an <b>activity</b>, and a service asking for it is a background
 * activity launch. The platform refuses that and reports it as a refusal by the
 * user, which is a different thing and reads exactly like one.
 *
 * <p>It is the same shape as {@link XrConsentActivity}, for the same underlying
 * reason: what the platform wants to show is an Activity, and the Go
 * application owns none. Two of these in one APK is not duplication — they ask
 * for different things, of different subsystems, with different lifetimes.
 *
 * <p>⚠ IT IS NOT EXPORTED. The device it opens is named by whoever starts it,
 * and an exported Activity would let any application on the phone drive this one
 * into asking for a device of their choosing under our name.
 */
public final class XrUsbPermissionActivity extends Activity {
    /** The device node to ask about, as {@link UsbDevice#getDeviceName}. */
    public static final String EXTRA_DEVICE_NAME = "org.goxrkit.android.DEVICE_NAME";

    /**
     * Whether the question has been put, so that a second {@link #onResume} —
     * which is exactly what returning from the system's dialog looks like —
     * does not ask again.
     */
    private boolean asked;

    /** True once we have paused at least once, i.e. the dialog took the screen. */
    private boolean dialogShown;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        String name = getIntent().getStringExtra(EXTRA_DEVICE_NAME);
        UsbManager um = getSystemService(UsbManager.class);
        UsbDevice dev = um == null ? null : um.getDeviceList().get(name);
        if (dev == null) {
            Log.e(XrWallService.TAG, "no USB device called " + name + " to ask about");
            deny(name);
            return;
        }
        if (um.hasPermission(dev)) {
            // Already allowed, so there is nothing to show. The answer still
            // goes back the same way: the service waits for a broadcast and
            // must not be left waiting for one that never comes.
            grant(dev);
        }
        // ⛔⛔ AND THE QUESTION IS NOT PUT HERE. See onResume.
    }

    /**
     * Asks, once, and only from a RESUMED activity.
     *
     * <h2>⛔⛔⛔ ASKING IN onCreate WAS DENIED IN SEVENTEEN MILLISECONDS</h2>
     *
     * The class comment above records that asking from the <i>Service</i> was
     * denied in eight, and the fix was to ask from an Activity. This Activity
     * then asked in {@link #onCreate} — and was denied in seventeen, with no
     * dialog shown, for the SAME underlying reason one level further in.
     * Measured on a Pixel 11 Pro Fold, 2026-10-31, from the platform's own log:
     *
     * <pre>
     * 21:52:03.803 wm_pause_activity:  XrDemoActivity, userLeaving=true
     * 21:52:03.809 onCreate XrUsbPermissionActivity   &lt;- requestPermission was here
     * 21:52:03.812 onResume XrUsbPermissionActivity   &lt;- two milliseconds LATER
     * 21:52:03.816 usb permission broadcast: granted=false
     * 21:52:03.824 Displayed XrUsbPermissionActivity: +21ms
     * </pre>
     *
     * The old Activity is paused at .803 and this one is resumed at .812, so
     * between those two instants <b>the application has no resumed activity at
     * all</b>. The question went into that window, the platform saw a
     * background request, and answered no without asking anybody — which is
     * indistinguishable from a refusal unless you time it. go-xrkit/android#38
     * is about that indistinguishability; this is its first measured instance.
     *
     * <p>So the question is put from {@code onResume}, and posted to the
     * decor view so it runs after the first frame rather than merely after the
     * lifecycle callback: "resumed" and "on screen" are not the same instant.
     */
    @Override
    protected void onResume() {
        super.onResume();
        if (asked && dialogShown) {
            // The dialog had the screen and has been dismissed, so this is the
            // activity it returned to. ⛔ WITHOUT THIS IT NEVER GOES AWAY: it is
            // transparent, so a stranded instance is invisible and still there,
            // and the next run finds a previous one "exiting" — which is what
            // the log of the failed attempt showed at 21:52:03.540.
            finish();
            overridePendingTransition(0, 0);
            return;
        }
        if (asked) {
            return;
        }
        String name = getIntent().getStringExtra(EXTRA_DEVICE_NAME);
        UsbManager um = getSystemService(UsbManager.class);
        UsbDevice dev = um == null ? null : um.getDeviceList().get(name);
        if (dev == null || um.hasPermission(dev)) {
            return; // onCreate has already answered for both of these.
        }
        asked = true;
        getWindow().getDecorView().post(() -> {
            PendingIntent pi = PendingIntent.getBroadcast(this, 0,
                    new Intent(XrWallService.ACTION_USB_PERMISSION).setPackage(getPackageName()),
                    // ⛔ MUTABLE, AND NOT BY PREFERENCE: UsbManager answers by ADDING
                    // EXTRA_PERMISSION_GRANTED and EXTRA_DEVICE to this intent, which
                    // it cannot do to an immutable one. UPDATE_CURRENT because a
                    // PendingIntent with the same request code is REUSED, and a stale
                    // one from a previous attempt would carry the previous device.
                    PendingIntent.FLAG_MUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
            Log.i(XrWallService.TAG, "asking for permission on " + name
                    + " from a resumed, drawn activity");
            um.requestPermission(dev, pi);
        });
    }

    @Override
    protected void onPause() {
        super.onPause();
        // ⛔⛔ FINISHING HERE WOULD TAKE THE DIALOG WITH IT. The system's dialog
        // is an activity drawn over this one, so its appearance is precisely
        // what pauses us: the first pause after asking means the dialog is UP,
        // and this Activity has to outlive it to be the thing it returns to.
        // The old code finished on every pause, which was harmless only
        // because no dialog was ever shown — see onResume.
        if (!asked) {
            finish();
            overridePendingTransition(0, 0);
            return;
        }
        dialogShown = true;
    }

    @Override
    protected void onStop() {
        super.onStop();
        // Stopped after the dialog had the screen: it has been answered one way
        // or the other, the broadcast is on its way to the service, and nothing
        // more is wanted from this Activity.
        if (dialogShown) {
            finish();
            overridePendingTransition(0, 0);
        }
    }

    /** Sends the answer the service is waiting for, so nothing waits forever. */
    private void deny(String name) {
        sendBroadcast(new Intent(XrWallService.ACTION_USB_PERMISSION)
                .setPackage(getPackageName())
                .putExtra(UsbManager.EXTRA_PERMISSION_GRANTED, false));
        finish();
    }

    private void grant(UsbDevice dev) {
        sendBroadcast(new Intent(XrWallService.ACTION_USB_PERMISSION)
                .setPackage(getPackageName())
                .putExtra(UsbManager.EXTRA_DEVICE, dev)
                .putExtra(UsbManager.EXTRA_PERMISSION_GRANTED, true));
        finish();
    }
}
