package bentoads

// Integration tests against the AdsGo_Testing PLC project
// (siyka/ads-go/plc/testing). Every output of Main.fbTypeTest is a
// deterministic function of nSeed, so each test writes a seed and checks what
// the ads input emits.
//
// Skipped unless ADS_TARGET_NET_ID is set, in the environment or in a .env
// file at the repo root (see .env.example):
//
//	go test -run Integration -v .

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	adsLib "github.com/RuneRoven/go-ads/v2"
)

const fb = "Main.fbTypeTest."

type plcEnv struct {
	ip, netID string
	port      int
	local     bool
}

// loadDotEnv sets KEY=VALUE pairs from path that are not already in the
// environment. A missing file is not an error.
func loadDotEnv(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			t.Setenv(k, v)
		}
	}
}

func integrationEnv(t *testing.T) plcEnv {
	t.Helper()
	loadDotEnv(t, ".env")
	netID := os.Getenv("ADS_TARGET_NET_ID")
	if netID == "" {
		t.Skip("ADS_TARGET_NET_ID not set (environment or .env)")
	}
	e := plcEnv{ip: "127.0.0.1", netID: netID, port: 851}
	if v := os.Getenv("ADS_TARGET_IP"); v != "" {
		e.ip = v
	}
	if v := os.Getenv("ADS_TARGET_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("ADS_TARGET_PORT: %v", err)
		}
		e.port = p
	}
	e.local, _ = strconv.ParseBool(os.Getenv("ADS_LOCAL_MODE"))
	return e
}

// controlSession opens a plain go-ads session used to drive the PLC.
func controlSession(t *testing.T, e plcEnv) *adsLib.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target, err := adsLib.NewAMSAddress(e.netID, uint16(e.port))
	if err != nil {
		t.Fatal(err)
	}
	var opts []adsLib.SessionOption
	if e.local {
		opts = append(opts, adsLib.WithLocalMode())
	}
	sess, err := adsLib.NewSession(context.Background(), adsLib.AMSEndpoint{IP: e.ip, Port: 48898, AMS: target}, opts...)
	if err != nil {
		t.Fatalf("control session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.Connect(ctx); err != nil {
		t.Fatalf("control connect: %v", err)
	}
	return sess
}

// setSeed stops auto-increment, writes nSeed and waits for the PLC to apply it.
func setSeed(t *testing.T, sess *adsLib.Session, seed uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sess.WriteToSymbol(ctx, fb+"bAutoMode", "false"); err != nil {
		t.Fatalf("write bAutoMode: %v", err)
	}
	want := strconv.FormatUint(uint64(seed), 10)
	if err := sess.WriteToSymbol(ctx, fb+"nSeed", want); err != nil {
		t.Fatalf("write nSeed: %v", err)
	}
	for ctx.Err() == nil {
		if v, err := sess.ReadFromSymbol(ctx, fb+"nUdintVar"); err == nil && v == want {
			if os.Getenv("ADS_TEST_TRACE") != "" {
				t.Logf("%s seed %d applied", time.Now().Format("05.000"), seed)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PLC did not apply seed %d", seed)
}

func newIntegrationInput(t *testing.T, e plcEnv, readType string, extra string, symbols []string) *adsCommInput {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "targetIP: %s\ntargetAMS: %s\nruntimePort: %d\nlocalMode: %t\n", e.ip, e.netID, e.port, e.local)
	if os.Getenv("ADS_TEST_TRACE") != "" {
		extra += "\nlogLevel: debug"
	}
	fmt.Fprintf(&b, "readType: %s\nintervalTime: 100\ncycleTime: 10\nmaxDelay: 0\n%s\nsymbols:\n", readType, extra)
	for _, s := range symbols {
		fmt.Fprintf(&b, "  - %q\n", s)
	}
	in := parseTestInput(t, b.String())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := in.Connect(ctx); err != nil {
		t.Fatalf("input Connect: %v", err)
	}
	t.Cleanup(func() { _ = in.Close(context.Background()) })
	return in
}

type sample struct {
	value                        string
	dataType, baseType, dataSize string
}

// collect reads batches until every symbol has been seen with accept()
// returning true, or the timeout passes. Keyed by symbol_name metadata.
func collect(t *testing.T, in *adsCommInput, symbols []string, accept func(name, value string) bool) map[string]sample {
	t.Helper()
	got := map[string]sample{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(got) < len(symbols) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		batch, ack, err := in.ReadBatch(ctx)
		cancel()
		if err != nil {
			t.Fatalf("ReadBatch: %v", err)
		}
		for _, msg := range batch {
			raw, _ := msg.AsBytes()
			name, _ := msg.MetaGet("symbol_name")
			if os.Getenv("ADS_TEST_TRACE") != "" {
				t.Logf("%s recv %s = %q", time.Now().Format("05.000"), name, raw)
			}
			if accept != nil && !accept(name, string(raw)) {
				continue
			}
			s := sample{value: string(raw)}
			s.dataType, _ = msg.MetaGet("data_type")
			s.baseType, _ = msg.MetaGet("base_type")
			s.dataSize, _ = msg.MetaGet("data_size")
			got[name] = s
		}
		if ack != nil {
			_ = ack(context.Background(), nil)
		}
	}
	return got
}

type scalarCase struct {
	field, dataType string
	size            int
	value           func(s uint32) string
}

func u(v uint64) string { return strconv.FormatUint(v, 10) }
func i(v int64) string  { return strconv.FormatInt(v, 10) }

// Expected values are what the PLC holds, written without reference to how
// go-ads formats them, so a lossy or wrong format shows up as a failure.
var scalarCases = []scalarCase{
	{"bBoolVar", "BOOL", 1, func(s uint32) string { return strconv.FormatBool(s%2 == 0) }},
	{"nSintVar", "SINT", 1, func(s uint32) string { return i(int64(int8(s))) }},
	{"nUsintVar", "USINT", 1, func(s uint32) string { return u(uint64(uint8(s))) }},
	{"nByteVar", "BYTE", 1, func(s uint32) string { return u(uint64(uint8(s))) }},
	{"nIntVar", "INT", 2, func(s uint32) string { return i(int64(int16(s))) }},
	{"nUintVar", "UINT", 2, func(s uint32) string { return u(uint64(uint16(s))) }},
	{"nWordVar", "WORD", 2, func(s uint32) string { return u(uint64(uint16(s))) }},
	{"nDintVar", "DINT", 4, func(s uint32) string { return i(int64(int32(s))) }},
	{"nUdintVar", "UDINT", 4, func(s uint32) string { return u(uint64(s)) }},
	{"nDwordVar", "DWORD", 4, func(s uint32) string { return u(uint64(s)) }},
	{"nLintVar", "LINT", 8, func(s uint32) string { return u(uint64(s)) }},
	{"nUlintVar", "ULINT", 8, func(s uint32) string { return u(uint64(s)) }},
	{"nLwordVar", "LWORD", 8, func(s uint32) string { return u(uint64(s)) }},
	{"fRealVar", "REAL", 4, func(s uint32) string { return strconv.FormatFloat(float64(float32(s)), 'f', -1, 32) }},
	{"fLrealVar", "LREAL", 8, func(s uint32) string { return strconv.FormatFloat(float64(s), 'f', -1, 64) }},
	// TIME is a duration in ms: hours must not wrap at 24.
	{"tTimeVar", "TIME", 4, func(s uint32) string { return clock(uint64(s), true) }},
	// TOD is ms since midnight: seconds must survive.
	{"tdTimeOfDayVar", "TIME_OF_DAY", 4, func(s uint32) string { return clock(uint64(s%86400000), true) }},
	{"dDateVar", "DATE", 4, func(s uint32) string { return time.Unix(int64(s), 0).UTC().Format("2006-01-02") }},
	{"dtDateTimeVar", "DATE_AND_TIME", 4, func(s uint32) string { return time.Unix(int64(s), 0).UTC().Format("2006-01-02 15:04:05") }},
	{"sStringVar", "STRING(255)", 256, func(s uint32) string { return "S=" + u(uint64(s)) }},
}

// clock renders ms as H:MM:SS[.mmm], hours unbounded, in the style go-ads
// uses for TIME below 24h.
func clock(ms uint64, withMs bool) string {
	h, rem := ms/3600000, ms%3600000
	m, rem := rem/60000, rem%60000
	sec, frac := rem/1000, rem%1000
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
	if withMs && frac != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%03d", frac), "0")
	}
	return out
}

// Seeds exercise sign wrap for the narrow types, a TIME past 24h, and a TOD
// with non-zero seconds and milliseconds.
var seeds = []uint32{0, 1, 200, 40000, 100_001, 90_061_001, 4_000_000_000}

func TestIntegrationScalarsInterval(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)

	symbols := make([]string, len(scalarCases))
	for k, c := range scalarCases {
		symbols[k] = fb + c.field
	}
	in := newIntegrationInput(t, e, "interval", "", symbols)

	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			setSeed(t, ctl, seed)
			want := u(uint64(seed))
			got := collect(t, in, symbols, nil)
			// The batch is read after setSeed returns, so nUdintVar proves freshness.
			if v := got[sanitize(fb+"nUdintVar")].value; v != want {
				t.Fatalf("stale batch: nUdintVar = %q, want %q", v, want)
			}
			checkScalars(t, seed, got)
		})
	}
}

func checkScalars(t *testing.T, seed uint32, got map[string]sample) {
	t.Helper()
	for _, c := range scalarCases {
		s, ok := got[sanitize(fb+c.field)]
		if !ok {
			t.Errorf("%s: no message", c.field)
			continue
		}
		if want := c.value(seed); s.value != want {
			t.Errorf("%s (%s): value %q, want %q", c.field, c.dataType, s.value, want)
		}
		if s.dataType != c.dataType {
			t.Errorf("%s: data_type %q, want %q", c.field, s.dataType, c.dataType)
		}
		if s.dataSize != strconv.Itoa(c.size) {
			t.Errorf("%s: data_size %q, want %d", c.field, s.dataSize, c.size)
		}
		if s.baseType == "" {
			t.Errorf("%s: base_type missing", c.field)
		}
	}
}

func TestIntegrationScalarsNotification(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	setSeed(t, ctl, 1)

	symbols := make([]string, len(scalarCases))
	for k, c := range scalarCases {
		symbols[k] = fb + c.field
	}
	in := newIntegrationInput(t, e, "notification", "", symbols)

	// Every symbol changes between these two seeds, so on-change
	// notifications must deliver all of them.
	const seed = 100_002
	setSeed(t, ctl, seed)
	got := collect(t, in, symbols, func(name, value string) bool {
		for _, c := range scalarCases {
			if sanitize(fb+c.field) == name {
				return value != c.value(1)
			}
		}
		return false
	})
	checkScalars(t, seed, got)
}

// The first value of each symbol is the PLC's initial sample on subscribe.
// Connect must not swallow it: the pipeline should see every symbol once
// even if nothing changes.
func TestIntegrationNotificationInitialSample(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	setSeed(t, ctl, 7)

	symbols := []string{fb + "nUdintVar", fb + "sStringVar"}
	in := newIntegrationInput(t, e, "notification", "", symbols)
	got := collect(t, in, symbols, nil)
	for _, s := range symbols {
		if _, ok := got[sanitize(s)]; !ok {
			t.Errorf("%s: initial value never reached ReadBatch", s)
		}
	}
}

// One misspelt symbol must not cost the whole poll.
func TestIntegrationIntervalPartialFailure(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	setSeed(t, ctl, 5)

	good := fb + "nUdintVar"
	in := newIntegrationInput(t, e, "interval", "", []string{good, fb + "doesNotExist"})
	got := collect(t, in, []string{good}, nil)
	if v := got[sanitize(good)].value; v != "5" {
		t.Errorf("%s = %q alongside a bad symbol, want \"5\"", good, v)
	}
}

func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("value is not a JSON object: %v\n%s", err, raw)
	}
	return m
}

func TestIntegrationStruct(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	const seed = 100_001
	setSeed(t, ctl, seed)

	sym := fb + "stStructVar"
	in := newIntegrationInput(t, e, "interval", "loadSymbols: true", []string{sym})
	got := collect(t, in, []string{sym}, nil)
	s, ok := got[sanitize(sym)]
	if !ok {
		t.Fatal("no message for struct")
	}
	t.Logf("data_type=%q base_type=%q data_size=%q", s.dataType, s.baseType, s.dataSize)
	t.Logf("value=%s", s.value)
	fields := decodeJSON(t, s.value)

	if v := fmt.Sprint(fields["nSeed"]); v != u(seed) {
		t.Errorf("nSeed = %s, want %d", v, seed)
	}
	for _, c := range scalarCases {
		v, ok := fields[c.field]
		if !ok {
			t.Errorf("struct field %s missing", c.field)
			continue
		}
		if got, want := fmt.Sprint(v), c.value(seed); got != want {
			t.Errorf("struct field %s (%s) = %s, want %s", c.field, c.dataType, got, want)
		}
	}
}

func TestIntegrationArrays(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	const seed = 32_760 // INT elements cross 32767 → -32768
	setSeed(t, ctl, seed)

	arr1, arr2d := fb+"aIntArray", fb+"aIntArray2d"
	in := newIntegrationInput(t, e, "interval", "loadSymbols: true", []string{arr1, arr2d})
	got := collect(t, in, []string{arr1, arr2d}, nil)

	check := func(sym string, want map[string]string) {
		s, ok := got[sanitize(sym)]
		if !ok {
			t.Errorf("%s: no message", sym)
			return
		}
		t.Logf("%s value=%s", sym, s.value)
		m := decodeJSON(t, s.value)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for idx, w := range want {
			v, ok := m[idx]
			if !ok {
				t.Errorf("%s: element %s missing (keys: %q)", sym, idx, keys)
				continue
			}
			if fmt.Sprint(v) != w {
				t.Errorf("%s%s = %v, want %s", sym, idx, v, w)
			}
		}
	}

	want1 := map[string]string{}
	for k := range uint32(10) {
		want1[fmt.Sprintf("[%d]", k)] = i(int64(int16(seed + k)))
	}
	check(arr1, want1)

	want2 := map[string]string{}
	for a := range uint32(3) {
		for b := range uint32(3) {
			want2[fmt.Sprintf("[%d,%d]", a, b)] = i(int64(int16(seed + a*3 + b)))
		}
	}
	check(arr2d, want2)
}
