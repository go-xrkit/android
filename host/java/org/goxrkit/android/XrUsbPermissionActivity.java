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
            return;
        }
        PendingIntent pi = PendingIntent.getBroadcast(this, 0,
                new Intent(XrWallService.ACTION_USB_PERMISSION).setPackage(getPackageName()),
                // ⛔ MUTABLE, AND NOT BY PREFERENCE: UsbManager answers by ADDING
                // EXTRA_PERMISSION_GRANTED and EXTRA_DEVICE to this intent, which
                // it cannot do to an immutable one. UPDATE_CURRENT because a
                // PendingIntent with the same request code is REUSED, and a stale
                // one from a previous attempt would carry the previous device.
                PendingIntent.FLAG_MUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        um.requestPermission(dev, pi);
        // The dialog is the system's, drawn over this Activity; finishing now
        // would take the dialog with it, so this waits to be dismissed.
    }

    @Override
    protected void onPause() {
        super.onPause();
        // The dialog has been answered one way or the other and the broadcast
        // is on its way; nothing more is wanted from this Activity.
        finish();
        overridePendingTransition(0, 0);
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
