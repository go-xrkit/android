// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android_test

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// manifest is the part of host/AndroidManifest.xml this suite judges.
//
// Only the activities, and only the attributes that decide whether an answer
// can reach us: this is a guard against one documented trap, not a second
// implementation of aapt2.
type manifest struct {
	App struct {
		Activities []struct {
			Name      string `xml:"http://schemas.android.com/apk/res/android name,attr"`
			NoHistory string `xml:"http://schemas.android.com/apk/res/android noHistory,attr"`
		} `xml:"activity"`
	} `xml:"application"`
}

// ⛔⛔ noHistory ON AN ACTIVITY THAT ASKS FOR A RESULT THROWS THE RESULT AWAY,
// and the documentation says so in one sentence:
//
//	"In this case, onActivityResult() is never called if you start another
//	 activity for a result from this activity."
//	 — developer.android.com, the <activity> manifest element
//
// XrConsentActivity exists for exactly one call, startActivityForResult on the
// MediaProjection consent dialog, and it carried noHistory="true" anyway.
//
// ⭐ MEASURED, AND IT COST A PERSON THEIR AFTERNOON'S ANSWER. On a Pixel 11 Pro
// Fold on 2026-11-01 the wearer tapped Start, the host logged `consent
// refused`, and xrdesk went back to waiting for a permission it had just been
// granted. The log blamed them for a decision they had not made.
//
// ⚠ XrUsbPermissionActivity KEEPS noHistory, and this test must not take it
// away: it asks through UsbManager.requestPermission with a broadcast
// PendingIntent, so its answer arrives at a receiver and not as an activity
// result. The trap is about results, so the rule is about the activity that
// asks for one — which is why this names it rather than banning the attribute.
func TestTheConsentActivityKeepsNoHistoryOff(t *testing.T) {
	t.Parallel()

	b, err := os.ReadFile("host/AndroidManifest.xml")
	if err != nil {
		t.Fatalf("the host manifest is what this asserts about: %v", err)
	}
	var m manifest
	if err := xml.Unmarshal(b, &m); err != nil {
		t.Fatalf("parsing the host manifest: %v", err)
	}
	// ⛔ A PARSE THAT FOUND NOTHING IS NOT A MANIFEST THAT PASSES. An empty
	// activity list would satisfy every assertion below, which is the shape of
	// a scan that could not read reporting zero findings.
	if len(m.App.Activities) == 0 {
		t.Fatal("no activities found in the host manifest, so this test asserted nothing")
	}

	const asksForAResult = "org.goxrkit.android.XrConsentActivity"
	var seen, control bool
	for _, a := range m.App.Activities {
		switch {
		case strings.HasSuffix(a.Name, "XrConsentActivity"):
			seen = true
			if a.NoHistory == "true" {
				t.Errorf("%s has noHistory=\"true\", and the documentation says that is how "+
					"onActivityResult stops being called — a granted consent then reads as a refusal",
					asksForAResult)
			}
		case strings.HasSuffix(a.Name, "XrUsbPermissionActivity"):
			// ⭐ THE POSITIVE CONTROL, and it is not decoration. An assertion
			// that an attribute is ABSENT passes just as well when the
			// attribute is never read at all -- one wrong namespace in the
			// struct tag above and NoHistory is "" for every activity, so the
			// rule would be green on a manifest that still carried the trap.
			// This activity keeps noHistory deliberately, so it must come back
			// "true": the check and the subject count alike.
			control = a.NoHistory == "true"
		}
	}
	if !seen {
		t.Fatalf("%s is not in the manifest, so this test asserted nothing about it",
			asksForAResult)
	}
	if !control {
		t.Fatal("XrUsbPermissionActivity did not come back with noHistory=\"true\", so this " +
			"test cannot read the attribute it is judging — the rule above proves nothing")
	}
}
