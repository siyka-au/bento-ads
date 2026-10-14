package bentoads

import (
	"context"
	"errors"

	"github.com/siyka-au/go-ads/v3/ams"
)

// Read result labels for the ads_reads counter.
const (
	readResultOK      = "ok"
	readResultPartial = "partial"
	readResultError   = "error"
)

// Bounded reasons for the ads_read_errors counter. Kept small and fixed so the
// metric's cardinality never grows with the PLC's vocabulary of ADS return
// codes -- a dashboard or alert written against these five values never needs
// to change.
const (
	readErrorTimeout  = "timeout"
	readErrorRouter   = "router"
	readErrorDevice   = "device"
	readErrorCanceled = "canceled"
	readErrorOther    = "other"
)

// allReadErrorReasons lists every value classifyReadError can return, for
// initialising the counter's label series at construction. Prometheus's
// increase() cannot see a counter's first increment if the series does not
// already exist at zero.
var allReadErrorReasons = []string{
	readErrorTimeout, readErrorRouter, readErrorDevice, readErrorCanceled, readErrorOther,
}

// classifyReadError maps a ReadValues failure to one of the reasons above.
//
// Order matters. ams.RouterError is checked before any ReturnCode check: it
// deliberately does not unwrap to ams.ReturnCode (see go-ads' RouterError.Is),
// specifically so a router-level rejection -- the request never reaching a
// service that could answer -- is never mistaken for a per-item PLC verdict.
// The incident this metric exists for was exactly a RouterError: "AMS router:
// 0x001A: TCP send error".
func classifyReadError(err error) string {
	if err == nil {
		return readErrorOther
	}
	if errors.Is(err, context.Canceled) {
		return readErrorCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return readErrorTimeout
	}
	var routerErr ams.RouterError
	if errors.As(err, &routerErr) {
		return readErrorRouter
	}
	// ADS-level timeouts the PLC or the sync client itself reports as a return
	// code, as opposed to the Go context deadline checked above. Checked only
	// after the RouterError branch: a router rejection can carry any code, and
	// must stay "router" rather than fall into "timeout" by coincidence of the
	// code it happened to carry.
	if errors.Is(err, ams.ReturnCodeDeviceTimeout) ||
		errors.Is(err, ams.ReturnCodeClientSyncTimeout) ||
		errors.Is(err, ams.ReturnCodeWsaeTimedOut) {
		return readErrorTimeout
	}
	var rc ams.ReturnCode
	if errors.As(err, &rc) {
		return readErrorDevice
	}
	return readErrorOther
}
