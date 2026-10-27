package bentoads

import (
	"context"
	"errors"
	"fmt"
	"testing"

	adsLib "github.com/nepionic/go-ads/v3"
	"github.com/nepionic/go-ads/v3/ams"
)

// TestClassifyReadError pins each bounded reason to the error shape it is
// meant to catch, including the incident that motivated this metric: a
// SumRead that fails with "context deadline exceeded" and then "AMS router:
// 0x001A: TCP send error" without go-ads ever declaring a disconnect.
func TestClassifyReadError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil is not classified as an error",
			err:  nil,
			want: readErrorOther,
		},
		{
			name: "context deadline exceeded is a timeout",
			err:  fmt.Errorf("SumRead failed: %w", context.DeadlineExceeded),
			want: readErrorTimeout,
		},
		{
			name: "context canceled is shutdown, not a failure",
			err:  fmt.Errorf("read aborted: %w", context.Canceled),
			want: readErrorCanceled,
		},
		{
			name: "the incident itself: AMS router TCP send error",
			err:  fmt.Errorf("outer: %w", ams.RouterError{Code: ams.ReturnCodeGlobalTcpSendError}),
			want: readErrorRouter,
		},
		{
			name: "a router rejection by any other global code is still router",
			err:  ams.RouterError{Code: ams.ReturnCodeGlobalTargetPortNotFound},
			want: readErrorRouter,
		},
		{
			name: "a per-item device return code is a device verdict",
			err:  fmt.Errorf("batch item: %w", ams.ReturnCodeDeviceSymbolNoFound),
			want: readErrorDevice,
		},
		{
			name: "an ADS-level device timeout return code is still a timeout",
			err:  ams.ReturnCodeDeviceTimeout,
			want: readErrorTimeout,
		},
		{
			name: "a sync client timeout return code is a timeout",
			err:  ams.ReturnCodeClientSyncTimeout,
			want: readErrorTimeout,
		},
		{
			name: "an unrelated error falls back to other",
			err:  errors.New("something else entirely"),
			want: readErrorOther,
		},
		{
			name: "go-ads' own disconnected sentinel falls back to other",
			err:  adsLib.ErrDisconnected,
			want: readErrorOther,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyReadError(tc.err); got != tc.want {
				t.Errorf("classifyReadError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyReadError_RouterPrecedesReturnCode pins the ordering rationale
// documented on classifyReadError: a RouterError must never be reported as
// "device" or "timeout" by virtue of the code number it happens to carry,
// because it is not a per-item PLC verdict at all (see go-ads'
// ams.RouterError.Is doc comment).
func TestClassifyReadError_RouterPrecedesReturnCode(t *testing.T) {
	// A RouterError carrying a code that ALSO appears in the timeout allow-list
	// below must still classify as "router", not "timeout".
	err := ams.RouterError{Code: ams.ReturnCodeDeviceTimeout}
	if got := classifyReadError(err); got != readErrorRouter {
		t.Errorf("classifyReadError(RouterError{DeviceTimeout}) = %q, want %q (router rejections never pass as device verdicts)",
			got, readErrorRouter)
	}
}

// TestPlcRunningValue pins the one state that counts as "running": only
// ams.StateRun, nothing else -- including states adjacent to it in the
// enumeration and the zero/invalid value.
func TestPlcRunningValue(t *testing.T) {
	tests := []struct {
		state ams.State
		want  int64
	}{
		{ams.StateRun, 1},
		{ams.StateConfig, 0},
		{ams.StateStop, 0},
		{ams.StateInvalid, 0},
		{ams.StateIdle, 0},
		{ams.State(250), 0},
	}
	for _, tc := range tests {
		if got := plcRunningValue(tc.state); got != tc.want {
			t.Errorf("plcRunningValue(%v) = %d, want %d", tc.state, got, tc.want)
		}
	}
}

// TestAllReadErrorReasons_CoversWhatClassifyReturns guards against the two
// lists silently diverging: every value classifyReadError can produce must be
// in the initialisation list, or that reason's series would never exist at
// zero and its first real increment would be invisible to increase().
func TestAllReadErrorReasons_CoversWhatClassifyReturns(t *testing.T) {
	want := map[string]bool{
		readErrorTimeout: false, readErrorRouter: false,
		readErrorDevice: false, readErrorCanceled: false, readErrorOther: false,
	}
	for _, r := range allReadErrorReasons {
		if _, ok := want[r]; !ok {
			t.Errorf("allReadErrorReasons contains unknown reason %q", r)
		}
		want[r] = true
	}
	for reason, seen := range want {
		if !seen {
			t.Errorf("allReadErrorReasons is missing %q", reason)
		}
	}
}
