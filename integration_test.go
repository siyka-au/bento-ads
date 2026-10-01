package bentoads

// Integration tests against the AdsClient_DeterministicTester PLC project,
// part of the AdsClient_Tester TwinCAT solution
// (https://github.com/siyka-au/ads-client-tester). Every output of Main.fbTypeTest is a
// deterministic function of nSeed, so each test writes a seed and checks the
// messages the ads input emits: their structured content against the Go value
// the PLC holds, converted as toBento converts it, and their metadata.
//
// Skipped unless ADS_TARGET_NET_ID is set, in the environment or in a .env
// file at the repo root (see .env.example):
//
//	go test -run Integration -v .

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	adsLib "github.com/siyka-au/go-ads/v3"
	"github.com/siyka-au/go-ads/v3/ams"
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

// controlSession opens a plain go-ads session used to drive the PLC, and
// restores nSeed and bAutoMode when the test ends.
func controlSession(t *testing.T, e plcEnv) *adsLib.Session {
	t.Helper()
	target, err := ams.NewAddress(e.netID, ams.Port(e.port))
	if err != nil {
		t.Fatal(err)
	}
	var opts []adsLib.Option
	if e.local {
		opts = append(opts, adsLib.WithLocalMode())
	}
	// NewSession's context bounds the session's lifetime; only Connect is timed.
	sess, err := adsLib.NewSession(context.Background(), adsLib.Endpoint{Host: e.ip, Port: 48898, Target: target}, opts...)
	if err != nil {
		t.Fatalf("control session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sess.Connect(ctx); err != nil {
		t.Fatalf("control connect: %v", err)
	}
	saved, err := sess.ReadValues(ctx, []string{fb + "nSeed", fb + "bAutoMode"})
	if err != nil {
		t.Fatalf("read seed state: %v", err)
	}
	t.Cleanup(func() {
		if _, err := sess.WriteValues(context.Background(), saved); err != nil {
			t.Errorf("restore nSeed/bAutoMode: %v", err)
		}
	})
	return sess
}

// setSeed stops auto-increment, writes nSeed, and waits until both the write
// (nSeed reads back) and a PLC cycle with it (nUdintVar, derived from nSeed)
// are visible.
func setSeed(t *testing.T, sess *adsLib.Session, seed uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sess.WriteValue(ctx, fb+"bAutoMode", false); err != nil {
		t.Fatalf("write bAutoMode: %v", err)
	}
	if err := sess.WriteValue(ctx, fb+"nSeed", seed); err != nil {
		t.Fatalf("write nSeed: %v", err)
	}
	for ctx.Err() == nil {
		v, err := sess.ReadValues(ctx, []string{fb + "nSeed", fb + "nUdintVar"})
		if err == nil && v[fb+"nSeed"] == seed && v[fb+"nUdintVar"] == seed {
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
	value                        any
	dataType, baseType, dataSize string
	rangeMin, rangeMax           string
}

// collect reads batches until every symbol has been seen with accept()
// returning true, or the timeout passes. Keyed by symbol_name metadata.
func collect(t *testing.T, in *adsCommInput, symbols []string, accept func(name string, value any) bool) map[string]sample {
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
			v, err := msg.AsStructured()
			if err != nil {
				t.Fatalf("message content is not structured: %v", err)
			}
			name, _ := msg.MetaGet("symbol_name")
			if os.Getenv("ADS_TEST_TRACE") != "" {
				t.Logf("%s recv %s = %#v", time.Now().Format("05.000"), name, v)
			}
			if accept != nil && !accept(name, v) {
				continue
			}
			s := sample{value: v}
			s.dataType, _ = msg.MetaGet("data_type")
			s.baseType, _ = msg.MetaGet("base_type")
			s.dataSize, _ = msg.MetaGet("data_size")
			s.rangeMin, _ = msg.MetaGet("range_min")
			s.rangeMax, _ = msg.MetaGet("range_max")
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
	// value is the Go value the PLC holds for seed s, as go-ads returns it.
	value func(s uint32) any
}

func unixDateTime(sec int64) civil.DateTime { return civil.DateTimeOf(time.Unix(sec, 0).UTC()) }

func timeOfDay(d time.Duration) civil.Time {
	return civil.Time{Hour: int(d / time.Hour), Minute: int(d % time.Hour / time.Minute),
		Second: int(d % time.Minute / time.Second), Nanosecond: int(d % time.Second)}
}

// data_type is what TwinCAT reports: the long names for TOD and DT, and STRING
// without its length (go-ads normalises STRING(n)).
var scalarCases = []scalarCase{
	{"bBoolVar", "BOOL", 1, func(s uint32) any { return s%2 == 0 }},
	{"nSintVar", "SINT", 1, func(s uint32) any { return int8(s) }},
	{"nUsintVar", "USINT", 1, func(s uint32) any { return uint8(s) }},
	{"nByteVar", "BYTE", 1, func(s uint32) any { return uint8(s) }},
	{"nIntVar", "INT", 2, func(s uint32) any { return int16(s) }},
	{"nUintVar", "UINT", 2, func(s uint32) any { return uint16(s) }},
	{"nWordVar", "WORD", 2, func(s uint32) any { return uint16(s) }},
	{"nDintVar", "DINT", 4, func(s uint32) any { return int32(s) }},
	{"nUdintVar", "UDINT", 4, func(s uint32) any { return s }},
	{"nDwordVar", "DWORD", 4, func(s uint32) any { return s }},
	{"nLintVar", "LINT", 8, func(s uint32) any { return int64(s) }},
	{"nUlintVar", "ULINT", 8, func(s uint32) any { return uint64(s) }},
	{"nLwordVar", "LWORD", 8, func(s uint32) any { return uint64(s) }},
	{"fRealVar", "REAL", 4, func(s uint32) any { return float32(s) }},
	{"fLrealVar", "LREAL", 8, func(s uint32) any { return float64(s) }},
	{"tTimeVar", "TIME", 4, func(s uint32) any { return time.Duration(s) * time.Millisecond }},
	{"tdTimeOfDayVar", "TIME_OF_DAY", 4, func(s uint32) any { return timeOfDay(time.Duration(s%86_400_000) * time.Millisecond) }},
	{"dDateVar", "DATE", 4, func(s uint32) any { return unixDateTime(int64(s)).Date }}, // UDINT_TO_DATE keeps the day
	{"dtDateTimeVar", "DATE_AND_TIME", 4, func(s uint32) any { return unixDateTime(int64(s)) }},
	{"tLtimeVar", "LTIME", 8, func(s uint32) any { return time.Duration(s) }},
	{"tdLTimeOfDayVar", "LTIME_OF_DAY", 8, func(s uint32) any { return timeOfDay(time.Duration(s)) }},
	{"dLDateVar", "LDATE", 8, func(s uint32) any { return civil.DateOf(time.Unix(0, int64(s)).UTC()) }},
	{"dtLDateTimeVar", "LDATE_AND_TIME", 8, func(s uint32) any { return civil.DateTimeOf(time.Unix(0, int64(s)).UTC()) }},
	{"sStringVar", "STRING", 256, func(s uint32) any { return "S=" + strconv.FormatUint(uint64(s), 10) }},
}

// nSubRangeCase covers Main.fbTypeTest.nSubRange, an INT(-10..10) subrange.
// It is top-level-only -- unlike every scalarCases entry, it is not a member
// of ST_TypeTestStruct -- so it is tracked separately rather than folded into
// scalarCases, which would break TestIntegrationStruct's per-field struct
// check.
var nSubRangeCase = scalarCase{"nSubRange", "INT", 2, func(s uint32) any { return int16(int32(s%21) - 10) }}

// nSubRangeMin/nSubRangeMax are nSubRange's declared bounds, expected as
// go-ads's recovered range_min/range_max metadata.
var nSubRangeMin, nSubRangeMax = ptrTo(int64(-10)), ptrTo(int64(10))

// topLevelCases is every top-level scalar under fbTypeTest: scalarCases (also
// shared with the struct-field check) plus nSubRangeCase.
var topLevelCases = append(append([]scalarCase{}, scalarCases...), nSubRangeCase)

func scalarSymbols() []string {
	out := make([]string, len(topLevelCases))
	for k, c := range topLevelCases {
		out[k] = fb + c.field
	}
	return out
}

// Seeds cover the sign wrap of each narrow type, a TIME over 24 h, a TOD with
// seconds and milliseconds, and the top of UDINT.
var seeds = []uint32{0, 1, 200, 40000, 100_001, 90_061_001, 4_000_000_000}

// wantRangeMeta gives the expected range_min/range_max metadata strings for
// field, or "" for a field with no declared subrange (every field except
// nSubRange).
func wantRangeMeta(field string) (min, max string) {
	if field != "nSubRange" {
		return "", ""
	}
	return strconv.FormatInt(*nSubRangeMin, 10), strconv.FormatInt(*nSubRangeMax, 10)
}

func checkScalars(t *testing.T, seed uint32, got map[string]sample) {
	t.Helper()
	for _, c := range topLevelCases {
		s, ok := got[sanitize(fb+c.field)]
		if !ok {
			t.Errorf("%s: no message", c.field)
			continue
		}
		if want := toBento(c.value(seed)); !reflect.DeepEqual(s.value, want) {
			t.Errorf("%s (%s): %#v (%T), want %#v (%T)", c.field, c.dataType, s.value, s.value, want, want)
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
		wantMin, wantMax := wantRangeMeta(c.field)
		if s.rangeMin != wantMin || s.rangeMax != wantMax {
			t.Errorf("%s: range_min/range_max = %q/%q, want %q/%q", c.field, s.rangeMin, s.rangeMax, wantMin, wantMax)
		}
	}
}

func TestIntegrationScalarsInterval(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	symbols := scalarSymbols()
	in := newIntegrationInput(t, e, "interval", "", symbols)

	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			setSeed(t, ctl, seed)
			// Only a batch read after the seed landed counts.
			got := collect(t, in, symbols, func(name string, v any) bool {
				return name != sanitize(fb+"nUdintVar") || v == seed
			})
			checkScalars(t, seed, got)
		})
	}
}

func TestIntegrationScalarsNotification(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	setSeed(t, ctl, 1)
	symbols := scalarSymbols()
	in := newIntegrationInput(t, e, "notification", "", symbols)

	// Every symbol changes between these two seeds, so on-change notifications
	// must deliver all of them; accept only the new values.
	const seed = 100_002
	setSeed(t, ctl, seed)
	got := collect(t, in, symbols, func(name string, v any) bool {
		for _, c := range topLevelCases {
			if sanitize(fb+c.field) == name {
				return reflect.DeepEqual(v, toBento(c.value(seed)))
			}
		}
		return false
	})
	checkScalars(t, seed, got)
}

// The first value of each symbol is the PLC's sample on subscribe. It must
// reach the pipeline even if nothing changes afterwards.
func TestIntegrationNotificationInitialSample(t *testing.T) {
	e := integrationEnv(t)
	ctl := controlSession(t, e)
	setSeed(t, ctl, 7)

	symbols := []string{fb + "nUdintVar", fb + "tTimeVar"}
	in := newIntegrationInput(t, e, "notification", "", symbols)
	got := collect(t, in, symbols, nil)
	want := map[string]any{
		sanitize(fb + "nUdintVar"): uint32(7),
		sanitize(fb + "tTimeVar"):  int64(7 * time.Millisecond),
	}
	for name, w := range want {
		if s, ok := got[name]; !ok {
			t.Errorf("%s: initial value never reached ReadBatch", name)
		} else if s.value != w {
			t.Errorf("%s = %#v, want %#v", name, s.value, w)
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
	if v := got[sanitize(good)].value; v != uint32(5) {
		t.Errorf("%s = %#v alongside a bad symbol, want uint32(5)", good, v)
	}
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
	if s.dataType != "ST_TypeTestStruct" {
		t.Errorf("data_type %q, want ST_TypeTestStruct", s.dataType)
	}
	fields, ok := s.value.(map[string]any)
	if !ok {
		t.Fatalf("struct content is %T, want map[string]any", s.value)
	}
	if fields["nSeed"] != uint32(seed) {
		t.Errorf("nSeed = %#v, want uint32(%d)", fields["nSeed"], seed)
	}
	for _, c := range scalarCases {
		if want := toBento(c.value(seed)); !reflect.DeepEqual(fields[c.field], want) {
			t.Errorf("stStructVar.%s = %#v (%T), want %#v (%T)", c.field, fields[c.field], fields[c.field], want, want)
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

	want1 := make([]any, 10)
	for k := range uint32(10) {
		want1[k] = int16(seed + k)
	}
	want2 := make([]any, 3)
	for a := range uint32(3) {
		row := make([]any, 3)
		for b := range uint32(3) {
			row[b] = int16(seed + a*3 + b)
		}
		want2[a] = row
	}
	for sym, want := range map[string][]any{arr1: want1, arr2d: want2} {
		if v := got[sanitize(sym)].value; !reflect.DeepEqual(v, want) {
			t.Errorf("%s = %#v, want %#v", sym, v, want)
		}
	}
}
