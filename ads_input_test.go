package bentoads

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	adsLib "github.com/siyka-au/go-ads/v3"
	"github.com/siyka-au/go-ads/v3/ams"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/warpstreamlabs/bento/public/service"
)

// newTestTracer returns a tracer backed by an in-memory SpanRecorder, for
// asserting on ads.read's name, attributes and status without a PLC.
func newTestTracer() (trace.Tracer, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return tp.Tracer("test"), sr
}

func attrsOf(t *testing.T, span sdktrace.ReadOnlySpan) map[string]any {
	t.Helper()
	m := make(map[string]any, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		m[string(kv.Key)] = kv.Value.AsInterface()
	}
	return m
}

func TestADSReadSpan_Success(t *testing.T) {
	tracer, sr := newTestTracer()
	_, span := startADSReadSpan(context.Background(), tracer, "1.2.3.4.1.1", "1.2.3.4", 851, 3)
	finishADSReadSpan(span, 3, nil)

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(ended))
	}
	got := ended[0]
	if got.Name() != "ads.read" {
		t.Errorf("span name = %q, want ads.read", got.Name())
	}

	attrs := attrsOf(t, got)
	want := map[string]any{
		"ads.target.ams_net_id": "1.2.3.4.1.1",
		"ads.target.ip":         "1.2.3.4",
		"ads.target.port":       int64(851),
		"ads.symbols.requested": int64(3),
		"ads.symbols.returned":  int64(3),
	}
	for k, wantV := range want {
		if attrs[k] != wantV {
			t.Errorf("attribute %s = %v, want %v", k, attrs[k], wantV)
		}
	}
	if got.Status().Code != codes.Unset {
		t.Errorf("status = %v, want Unset", got.Status().Code)
	}

	// Messages built from this read attach a background context carrying the
	// span, per ReadBatchPull -- check that context round-trips the
	// SpanContext and that the span is no longer recording, which is what lets
	// Bento's InitSpan start input_ads as ads.read's child instead of reusing it.
	msgCtx := trace.ContextWithSpan(context.Background(), span)
	fromMsg := trace.SpanFromContext(msgCtx)
	wantSC := span.SpanContext()
	gotSC := fromMsg.SpanContext()
	if gotSC.TraceID() != wantSC.TraceID() || gotSC.SpanID() != wantSC.SpanID() {
		t.Error("message context does not carry the read span's SpanContext")
	}
	if fromMsg.IsRecording() {
		t.Error("span is still recording after finishADSReadSpan, want ended")
	}
}

func TestADSReadSpan_FatalErrorNoValues(t *testing.T) {
	tracer, sr := newTestTracer()
	_, span := startADSReadSpan(context.Background(), tracer, "1.2.3.4.1.1", "1.2.3.4", 851, 2)
	readErr := errors.New("transport failure")
	finishADSReadSpan(span, 0, readErr)

	got := sr.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", got.Status().Code)
	}
	if got.Status().Description != readErr.Error() {
		t.Errorf("status description = %q, want %q", got.Status().Description, readErr.Error())
	}
	if len(got.Events()) == 0 {
		t.Error("RecordError did not add an event to the span")
	}
}

// A partial BatchError with some values still returned must be recorded on
// the span but must not fail its status -- some data beats none.
func TestADSReadSpan_PartialBatchErrorStillReturnsStatusOK(t *testing.T) {
	tracer, sr := newTestTracer()
	_, span := startADSReadSpan(context.Background(), tracer, "1.2.3.4.1.1", "1.2.3.4", 851, 3)
	batchErr := &adsLib.BatchError{
		Op: "read", Requested: 3, Succeeded: 2,
		Items: []adsLib.BatchItemError{{Symbol: "MAIN.x"}},
	}
	finishADSReadSpan(span, 2, batchErr)

	got := sr.Ended()[0]
	if got.Status().Code == codes.Error {
		t.Error("partial batch with some values returned marked the span failed, want Unset")
	}
	if len(got.Events()) == 0 {
		t.Error("partial batch error was not recorded on the span")
	}
	attrs := attrsOf(t, got)
	if attrs["ads.symbols.failed"] != int64(1) {
		t.Errorf("ads.symbols.failed = %v, want 1", attrs["ads.symbols.failed"])
	}
}

// A BatchError where nothing came back at all must fail the span: all data
// is missing, so this is indistinguishable from a wholly failed read.
func TestADSReadSpan_BatchErrorNoValuesReturnedFailsSpan(t *testing.T) {
	tracer, sr := newTestTracer()
	_, span := startADSReadSpan(context.Background(), tracer, "1.2.3.4.1.1", "1.2.3.4", 851, 2)
	batchErr := &adsLib.BatchError{
		Op: "read", Requested: 2, Succeeded: 0,
		Items: []adsLib.BatchItemError{{Symbol: "MAIN.x"}, {Symbol: "MAIN.y"}},
	}
	finishADSReadSpan(span, 0, batchErr)

	got := sr.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Errorf("status code = %v, want Error", got.Status().Code)
	}
}

func parseTestInput(t *testing.T, yaml string) *adsCommInput {
	t.Helper()
	conf, err := adsConf.ParseYAML(yaml, nil)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	var opts []service.MockResourcesOptFn
	if os.Getenv("ADS_TEST_TRACE") != "" {
		opts = append(opts, service.MockResourcesOptUseSlogger(
			slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	}
	in, err := adsCommInputFromConfig(conf, service.MockResources(opts...))
	if err != nil {
		t.Fatalf("build input: %v", err)
	}
	return in
}

const minimalConfig = `
targetIP: 127.0.0.1
targetAMS: 127.0.0.1.1.1
symbols: [ MAIN.x ]
`

func TestConfigDefaults(t *testing.T) {
	in := parseTestInput(t, minimalConfig)
	if in.runtimePort != 851 {
		t.Errorf("runtimePort = %d, want 851 (TwinCAT 3)", in.runtimePort)
	}
	if in.readType != "notification" {
		t.Errorf("readType = %q, want notification", in.readType)
	}
	if in.localMode {
		t.Error("localMode defaults to true, want false")
	}
	if in.transmissionMode != ams.TransModeServerOnChange {
		t.Errorf("transmissionMode = %v, want serverOnChange", in.transmissionMode)
	}
}

// logLevel defaults to warn (not disabled) so go-ads errors such as reconnect
// failures reach Bento's logger by default.
func TestConfigDefaults_LogLevel(t *testing.T) {
	in := parseTestInput(t, minimalConfig)
	if !in.adsLogger.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("default logLevel does not enable warn records")
	}
	if in.adsLogger.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("default logLevel enables info records, want warn and above only")
	}
}

func TestConfigLogLevel_DisabledStillAccepted(t *testing.T) {
	in := parseTestInput(t, minimalConfig+"logLevel: disabled\n")
	if in.adsLogger.Enabled(context.Background(), slog.LevelError) {
		t.Error("logLevel: disabled still enables error records")
	}
}

func TestConfigTransmissionModes(t *testing.T) {
	for name, want := range map[string]ams.TransMode{
		"serverOnChange":  ams.TransModeServerOnChange,
		"serverCycle":     ams.TransModeServerCycle,
		"serverOnChange2": ams.TransModeServerOnChange2,
		"serverCycle2":    ams.TransModeServerCycle2,
	} {
		in := parseTestInput(t, minimalConfig+"transmissionMode: "+name+"\n")
		if in.transmissionMode != want {
			t.Errorf("transmissionMode %q = %v, want %v", name, in.transmissionMode, want)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for name, yaml := range map[string]string{
		"bad targetIP":         "targetIP: 1.2.3\ntargetAMS: 1.2.3.4.1.1\nsymbols: [a]",
		"bad targetAMS":        "targetIP: 1.2.3.4\ntargetAMS: 1.2.3.4.1\nsymbols: [a]",
		"bad readType":         "targetIP: 1.2.3.4\ntargetAMS: 1.2.3.4.1.1\nreadType: poll\nsymbols: [a]",
		"bad hostAMS":          "targetIP: 1.2.3.4\ntargetAMS: 1.2.3.4.1.1\nhostAMS: 1.2.3.999.1.1\nsymbols: [a]",
		"IPv6 targetIP":        "targetIP: \"::1\"\ntargetAMS: 1.2.3.4.1.1\nsymbols: [a]",
		"bad transmissionMode": "targetIP: 1.2.3.4\ntargetAMS: 1.2.3.4.1.1\ntransmissionMode: onChange\nsymbols: [a]",
	} {
		t.Run(name, func(t *testing.T) {
			conf, err := adsConf.ParseYAML(yaml, nil)
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			if _, err := adsCommInputFromConfig(conf, service.MockResources()); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestCreateSymbolList(t *testing.T) {
	got := createSymbolList([]string{"MAIN.a", "MAIN.b:50:100", "MAIN.c:x:y"}, 1000, 100)
	want := []plcSymbol{
		{"MAIN.a", 100 * time.Millisecond, 1000 * time.Millisecond},
		{"MAIN.b", 50 * time.Millisecond, 100 * time.Millisecond},
		{"MAIN.c", 100 * time.Millisecond, 1000 * time.Millisecond},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d symbols, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("symbol %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Close must unblock a notification read that is waiting for data, and every
// read after it must report end of input rather than block.
func TestCloseUnblocksNotificationRead(t *testing.T) {
	in := parseTestInput(t, minimalConfig)

	errc := make(chan error, 1)
	go func() {
		_, _, err := in.ReadBatch(context.Background())
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := in.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, service.ErrEndOfInput) {
			t.Errorf("ReadBatch after Close = %v, want ErrEndOfInput", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadBatch did not return within 1s of Close")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, err := in.ReadBatch(context.Background())
		if !errors.Is(err, service.ErrEndOfInput) {
			t.Errorf("second ReadBatch after Close = %v, want ErrEndOfInput", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second ReadBatch blocked after Close")
	}
}

func TestConnectAfterClose(t *testing.T) {
	in := parseTestInput(t, minimalConfig)
	_ = in.Close(context.Background())
	if err := in.Connect(context.Background()); !errors.Is(err, service.ErrEndOfInput) {
		t.Errorf("Connect after Close = %v, want ErrEndOfInput", err)
	}
}

// Concurrent Close calls must not panic on a double channel close. Run with
// -race to also check the handler and done accesses.
func TestConcurrentClose(t *testing.T) {
	in := parseTestInput(t, minimalConfig)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = in.Close(context.Background())
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, _, _ = in.ReadBatch(ctx)
		}()
	}
	wg.Wait()
}

// onADSDisconnect/onADSReconnect back go-ads's WithOnDisconnect/WithOnReconnect
// callbacks, which must never block. There is no PLC here to drive an actual
// disconnect/reconnect, and the public service.Metrics API exposes no way to
// read a counter back, so this only checks the wiring itself: the methods
// exist on the input, are safe to call directly (nil-safe counters, no
// panics), and return immediately.
func TestADSDisconnectReconnectCallbacks_DoNotBlock(t *testing.T) {
	in := parseTestInput(t, minimalConfig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		in.onADSDisconnect()
		in.onADSReconnect()
		in.onADSDisconnect()
		in.onADSReconnectAttempt(1, errors.New("dial failed"))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onADSDisconnect/onADSReconnect/onADSReconnectAttempt blocked")
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestSymbolMetaFrom_Range(t *testing.T) {
	t.Run("with range", func(t *testing.T) {
		m := symbolMetaFrom(adsLib.SymbolView{DataType: "INT", Length: 2, RangeMin: ptrTo(int64(-10)), RangeMax: ptrTo(int64(10))})
		if m.rangeMin == nil || m.rangeMax == nil || *m.rangeMin != -10 || *m.rangeMax != 10 {
			t.Errorf("rangeMin/rangeMax = %v/%v, want -10/10", m.rangeMin, m.rangeMax)
		}
	})
	t.Run("without range", func(t *testing.T) {
		m := symbolMetaFrom(adsLib.SymbolView{DataType: "INT", Length: 2})
		if m.rangeMin != nil || m.rangeMax != nil {
			t.Errorf("rangeMin/rangeMax = %v/%v, want nil/nil", m.rangeMin, m.rangeMax)
		}
	})
}

// makeMessage must carry the sample time as an RFC 3339 "timestamp" metadata
// key, so the body stays unchanged and the pipeline can map it.
func TestMakeMessage_TimestampMetadata(t *testing.T) {
	in := parseTestInput(t, minimalConfig)
	sampled := time.Date(2026, 10, 5, 12, 0, 0, 123456789, time.UTC)

	msg := in.makeMessage("MAIN.x", int16(5), sampled)
	v, ok := msg.MetaGet("timestamp")
	if !ok {
		t.Fatal("timestamp metadata absent")
	}
	got, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC 3339: %v", v, err)
	}
	if !got.Equal(sampled) {
		t.Errorf("timestamp = %v, want %v", got, sampled)
	}
}

func TestMakeMessage_RangeMetadataOnlyWhenPresent(t *testing.T) {
	in := parseTestInput(t, minimalConfig)

	t.Run("with range", func(t *testing.T) {
		in.meta = map[string]symbolMeta{"MAIN.x": {dataType: "INT", size: 2, rangeMin: ptrTo(int64(-10)), rangeMax: ptrTo(int64(10))}}
		msg := in.makeMessage("MAIN.x", int16(5), time.Time{})
		if v, ok := msg.MetaGet("range_min"); !ok || v != "-10" {
			t.Errorf("range_min = %q, %v, want \"-10\", true", v, ok)
		}
		if v, ok := msg.MetaGet("range_max"); !ok || v != "10" {
			t.Errorf("range_max = %q, %v, want \"10\", true", v, ok)
		}
	})
	t.Run("without range", func(t *testing.T) {
		in.meta = map[string]symbolMeta{"MAIN.x": {dataType: "INT", size: 2}}
		msg := in.makeMessage("MAIN.x", int16(5), time.Time{})
		if _, ok := msg.MetaGet("range_min"); ok {
			t.Error("range_min present, want absent for a symbol with no declared subrange")
		}
		if _, ok := msg.MetaGet("range_max"); ok {
			t.Error("range_max present, want absent for a symbol with no declared subrange")
		}
	})
}
