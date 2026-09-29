package bentoads

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/warpstreamlabs/bento/public/service"
)

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
