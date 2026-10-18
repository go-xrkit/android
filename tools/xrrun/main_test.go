// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// ⛔⛔ THIS COST THREE MEASUREMENT CYCLES, EVERY ONE OF THEM AT THE END. Behind
// the keyguard Android does not credit the application as foreground, however
// resumed its Activity is, so the host's own service start throws and the
// process dies in onResume — after a forty-second build and an install. The
// transcripts the fleet keeps of those runs are the fixtures here.
//
// ⛔⛔ THE FIXTURE BELOW IS WHAT THE PHONE PRINTED, not what I expected it to.
// The first version of this test was written from the crash transcript and
// asserted `mWakefulness=Dozing` against `dumpsys window policy` — a string
// that section never prints. Ten expectations, all green, and the guard would
// have passed a phone that was asleep at that moment. Pinning one real
// transcript is the control; see the fleet note about hand-written
// expectations.
const asleepAndLocked = `  mWakefulness=Dozing
  mWakefulnessChanging=false
  mHalInteractiveModeEnabled=false
    mIsInteractive: false
    mKeyguardOccluded=false mPendingKeyguardOccluded=false
    KeyguardServiceDelegate
      showing=true
      inputRestricted=true
      occluded=false
      screenState=SCREEN_STATE_ON
      interactiveState=INTERACTIVE_STATE_SLEEP
`

// ⛔⛔⛔ AND THIS ONE IS THE SAME PHONE, AWAKE AND UNLOCKED — the fixture that
// proves the guard was reading a SCREEN where it meant the PHONE. Measured on
// the Pixel 11 Pro Fold, 2026-10-18 21:05 CEST, while a person was looking at
// it: `mIsInteractive: false` for the dark screen, `: true` for the one in use,
// and `interactiveState=INTERACTIVE_STATE_SLEEP` for a display of its own. The
// old guard found the first of those and refused the run three times, telling a
// person to wake a phone that was already awake.
//
// The numbered `mWakefulness=` lines are kept in the fixture deliberately: they
// are per-power-group statistics, and a reader that took the first match would
// get `0` and have to guess what that means.
const awakeAndUnlockedOnAFoldable = `  mWakefulness=Awake
  mWakefulnessChanging=false
  mHalInteractiveModeEnabled=true
  Wakefulness Session Power Group powerGroupId: 0
    current wakefulness: 0
    mIsInteractive: false
  Wakefulness Session Power Group powerGroupId: 1
    current wakefulness: 1
    mIsInteractive: true
  mWakefulness=0
  mWakefulness=1
    KeyguardServiceDelegate
      showing=false
      inputRestricted=false
      interactiveState=INTERACTIVE_STATE_SLEEP
`

func TestAPhoneThatCannotRunAProbeSaysSo(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want []string
	}{
		{
			// Measured on the Pixel 11 Pro Fold, 2026-10-11 18:25 CEST.
			name: "asleep and locked, as the phone itself reports it",
			in:   asleepAndLocked,
			want: []string{"it is asleep", "it is locked"},
		},
		{
			// ⛔ THE CASE A WAKEFULNESS-ONLY CHECK WOULD HAVE PASSED: the phone
			// is awake, in a pocket under the lock screen, and the service
			// start is refused all the same.
			name: "awake but locked",
			in:   "  mWakefulness=Awake\n    mIsInteractive: true\n      inputRestricted=true\n",
			want: []string{"it is locked"},
		},
		{
			name: "asleep with the keyguard not restricting",
			in:   "  mWakefulness=Asleep\n      inputRestricted=false\n",
			want: []string{"it is asleep"},
		},
		{
			name: "usable",
			in: "  mWakefulness=Awake\n    mIsInteractive: true\n" +
				"      showing=false\n      inputRestricted=false\n" +
				"      interactiveState=INTERACTIVE_STATE_AWAKE\n",
			want: nil,
		},
		{
			// ⛔ AND THE TRAP THE MEASUREMENT EXPOSED: the screen is ON while
			// this phone sleeps, because of the always-on display. A check that
			// read the screen would have called the fixture above usable.
			name: "the screen is on and the phone is still asleep",
			in:   "  mWakefulness=Dozing\n      screenState=SCREEN_STATE_ON\n",
			want: []string{"it is asleep"},
		},
		{
			// ⛔⛔⛔ THE DEFECT THIS FILE EXISTS FOR, SECOND EDITION: one screen of
			// a foldable is dark while the phone is plainly awake and unlocked,
			// and the guard refused it three times running.
			name: "awake and unlocked, with one screen of the fold dark",
			in:   awakeAndUnlockedOnAFoldable,
			want: nil,
		},
		{
			// A phone genuinely asleep still says so, even though the SAME
			// per-screen disagreement is present — the device-wide line decides,
			// and this is the control that says the fix did not simply stop
			// looking.
			name: "asleep, with one screen still reporting interactive",
			in: "  mWakefulness=Dozing\n    mIsInteractive: false\n" +
				"    mIsInteractive: true\n",
			want: []string{"it is asleep"},
		},
		{
			// ⚠ The fallback, for the day the section is renamed: per-screen
			// signs are all there is, and they agree.
			name: "no device-wide line, and every screen is down",
			in:   "  something renamed\n    mIsInteractive: false\n",
			want: []string{"it is asleep"},
		},
		{
			// ⚠ And the same fallback when they disagree: one screen up is
			// enough for the platform, so this is not a refusal.
			name: "no device-wide line, and one screen is up",
			in: "  something renamed\n    mIsInteractive: false\n" +
				"      interactiveState=INTERACTIVE_STATE_AWAKE\n",
			want: nil,
		},
		{
			// ⚠ AN UNREADABLE SECTION IS NOT A REFUSAL. dumpsys gets renamed
			// between releases, and a tool that refused to run because it could
			// not find a field would be worse than the crash it avoids — the
			// crash at least names its own cause.
			name: "a section this tool does not recognise",
			in:   "Window Manager Policy state:\n  something else entirely\n",
			want: nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := lockedBecause(c.in)
			if !slices.Equal(got, c.want) {
				t.Fatalf("lockedBecause = %q, want %q", got, c.want)
			}
		})
	}
}

// ⛔⛔ THE SUGGESTED COMMAND MUST NAME EVERY POWER SOURCE. It said `stayon usb`
// for four releases, which sets one bit of stay_on_while_plugged_in — and the
// rig this tool exists for reports `AC powered: true, USB powered: false`,
// because the phone is charged through the same hub the glasses hang off. The
// setting read back as applied while the screen kept going dark on a 30-second
// timeout, which is the worst shape a suggestion can have: it looks like a fix
// that was tried. `true` sets all of them.
func TestTheRefusalSuggestsSomethingThatWorksOnThisRig(t *testing.T) {
	err := refusal("a-device", []string{"it is asleep"})
	if !strings.Contains(err.Error(), "svc power stayon true") {
		t.Errorf("the refusal does not suggest stayon true:\n%v", err)
	}
	if strings.Contains(err.Error(), "stayon usb") {
		t.Errorf("the refusal still suggests stayon usb, which does nothing when the "+
			"supply is classified as AC:\n%v", err)
	}
}

// ⛔⛔ THE GUARD WAITS FOR THE GESTURE INSTEAD OF REFUSING IT. Unlocking is a
// fingerprint or a code: no tool can do it and none should, so a guard that
// refuses the instant it is called makes a person time an unlock to a launch.
// That cost five round trips for one unlock on 2026-10-18.
//
// stateOf is replaced here because a test with a phone would need the gesture
// it exists to wait for.
func TestTheGuardWaitsForTheHandThatHasToUnlockIt(t *testing.T) {
	restore := stateOf
	t.Cleanup(func() { stateOf = restore })

	asks := 0
	stateOf = func(context.Context, string) []string {
		asks++
		if asks < 3 {
			return []string{"it is locked"}
		}
		return nil // somebody unlocked it
	}
	if err := awaitAwake(context.Background(), "a-device", time.Minute); err != nil {
		t.Fatalf("awaitAwake = %v, want it to carry on once the phone is unlocked", err)
	}
	if asks < 3 {
		t.Errorf("the phone was asked %d time(s); it must be asked AGAIN after the "+
			"first refusal, or nothing is being waited for", asks)
	}
}

// And it still gives up, with the reason the phone last gave, rather than
// waiting for somebody who is not coming.
func TestTheGuardGivesUpOnAPhoneNobodyTouches(t *testing.T) {
	restore := stateOf
	t.Cleanup(func() { stateOf = restore })
	stateOf = func(context.Context, string) []string { return []string{"it is asleep"} }

	err := awaitAwake(context.Background(), "a-device", time.Nanosecond)
	if err == nil {
		t.Fatal("awaitAwake = nil for a phone that never woke")
	}
	if !strings.Contains(err.Error(), "it is asleep") {
		t.Errorf("the refusal does not name what the phone last said:\n%v", err)
	}
}

// ⛔ AND AN ALREADY-USABLE PHONE IS NOT MADE TO WAIT A SECOND. The wait polls on
// a one-second tick, and paying that on every run of every probe is a cost no
// measurement needs.
func TestAUsablePhoneIsNotDelayed(t *testing.T) {
	restore := stateOf
	t.Cleanup(func() { stateOf = restore })
	stateOf = func(context.Context, string) []string { return nil }

	start := time.Now()
	if err := awaitAwake(context.Background(), "a-device", time.Minute); err != nil {
		t.Fatalf("awaitAwake = %v on a usable phone", err)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Errorf("a usable phone was delayed %v; the first look must be enough", waited)
	}
}

// A cancelled context ends the wait, because a person who has stopped the run
// has answered the question.
func TestCancellingTheRunEndsTheWait(t *testing.T) {
	restore := stateOf
	t.Cleanup(func() { stateOf = restore })
	stateOf = func(context.Context, string) []string { return []string{"it is locked"} }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := awaitAwake(ctx, "a-device", time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("awaitAwake = %v, want context.Canceled", err)
	}
}

// Each reason must appear once, or a phone that is both asleep and locked is
// described twice and the message reads as two separate problems. The real
// transcript trips several signs for each reason, which is why this matters.
func TestEachReasonIsGivenOnce(t *testing.T) {
	got := lockedBecause(asleepAndLocked + "isKeyguardShowing=true\nmShowingLockscreen=true\n")
	if len(got) != 2 {
		t.Fatalf("lockedBecause = %q, want exactly the two reasons", got)
	}
}

// The transcript table is how -app alone is enough, and a name missing from it
// reads as "the probe wrote nothing" — which is also what a crash looks like.
func TestEveryProbeInThisRepositoryHasATranscript(t *testing.T) {
	for _, probe := range []string{"xrusb", "xrhead", "xrcam", "xrscreen", "xrglasses"} {
		if _, ok := transcripts[probe]; !ok {
			t.Errorf("%s writes no transcript this tool knows", probe)
		}
	}
	for probe, file := range transcripts {
		if !strings.HasSuffix(file, ".txt") {
			t.Errorf("%s writes %q, which is not a transcript this tool reads", probe, file)
		}
	}
}

func TestFirstLinesStopsWhereItIsAsked(t *testing.T) {
	// A crash dump is hundreds of lines and the first few name the cause; the
	// rest is the platform's own stack.
	in := []byte("one\ntwo\nthree\nfour\nfive")
	if got := firstLines(in, 3); got != "one\ntwo\nthree" {
		t.Errorf("firstLines = %q", got)
	}
	if got := firstLines(in, 99); got != string(in) {
		t.Errorf("firstLines past the end = %q, want all of it", got)
	}
}
