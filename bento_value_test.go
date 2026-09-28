package bentoads

import (
	"reflect"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"github.com/warpstreamlabs/bento/public/bloblang"

	// Registers the ts_* and parse_duration methods the mapping below uses.
	_ "github.com/warpstreamlabs/bento/public/components/pure"
)

func TestToBento(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want any
	}{
		// Types Bloblang knows pass through untouched.
		{"bool", true, true},
		{"int16", int16(-5), int16(-5)},
		{"uint64", uint64(1 << 63), uint64(1 << 63)},
		{"float32", float32(1.5), float32(1.5)},
		{"string", "S=7", "S=7"},

		{"DT", civil.DateTime{Date: civil.Date{Year: 2024, Month: 6, Day: 15}, Time: civil.Time{Hour: 13, Minute: 30, Nanosecond: 5}},
			time.Date(2024, 6, 15, 13, 30, 0, 5, time.UTC)},
		{"DATE", civil.Date{Year: 1900, Month: 6, Day: 15}, time.Date(1900, 6, 15, 0, 0, 0, 0, time.UTC)},
		{"TIME", 25*time.Hour + time.Millisecond, int64(90_000_001_000_000)},
		{"TOD", civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999_999_999}, int64(86_399_999_999_999)},

		{"struct", map[string]any{"n": int16(1), "t": time.Second, "d": civil.Date{Year: 1970, Month: 1, Day: 2}},
			map[string]any{"n": int16(1), "t": int64(1e9), "d": time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)}},
		{"2D array", []any{[]any{time.Millisecond}, []any{civil.Time{Second: 1}}},
			[]any{[]any{int64(1e6)}, []any{int64(1e9)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toBento(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("toBento(%#v) = %#v (%T), want %#v (%T)", tt.in, got, got, tt.want, tt.want)
			}
		})
	}
}

// What toBento produces must be usable in Bloblang as the types it claims:
// timestamps through ts_* methods and nanosecond counts as numbers.
func TestToBentoValuesWorkInBloblang(t *testing.T) {
	converted := toBento(map[string]any{
		"dt":   civil.DateTime{Date: civil.Date{Year: 2024, Month: 6, Day: 15}, Time: civil.Time{Hour: 13, Minute: 30, Nanosecond: 1_000}},
		"date": civil.Date{Year: 2024, Month: 2, Day: 29},
		"time": 25*time.Hour + time.Minute + time.Second + time.Millisecond,
		"tod":  civil.Time{Hour: 13, Minute: 45, Second: 0, Nanosecond: 1_000_000},
	})
	exe, err := bloblang.Parse(`
root.dt_rfc3339 = this.dt.ts_format("2006-01-02T15:04:05.999999999Z07:00")
root.dt_unix_ns = this.dt.ts_unix_nano()
root.date = this.date.ts_format("2006-01-02")
root.time_equals_parsed = this.time == "25h1m1.001s".parse_duration()
root.time_ms = this.time / 1000000
root.tod_ms = this.tod / 1000000
`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exe.Query(converted)
	if err != nil {
		t.Fatalf("mapping failed on converted values: %v", err)
	}
	want := map[string]any{
		"dt_rfc3339":         "2024-06-15T13:30:00.000001Z",
		"dt_unix_ns":         time.Date(2024, 6, 15, 13, 30, 0, 1_000, time.UTC).UnixNano(),
		"date":               "2024-02-29",
		"time_equals_parsed": true,
		// Bloblang's "/" always yields a float64.
		"time_ms": float64(90_061_001),
		"tod_ms":  float64(49_500_001),
	}
	for k, w := range want {
		if g := got.(map[string]any)[k]; g != w {
			t.Errorf("%s = %#v (%T), want %#v (%T)", k, g, g, w, w)
		}
	}
}

// The reason for converting at all: Bloblang does not understand the Go types
// go-ads uses for durations and civil dates.
func TestUnconvertedValuesAreOpaqueToBloblang(t *testing.T) {
	for name, v := range map[string]any{
		"time.Duration":  time.Second,
		"civil.DateTime": civil.DateTime{Date: civil.Date{Year: 2024, Month: 1, Day: 1}},
	} {
		exe, err := bloblang.Parse(`root = this.v + 1`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := exe.Query(map[string]any{"v": v}); err == nil {
			t.Errorf("%s: expected Bloblang to reject arithmetic on it; conversion may no longer be needed", name)
		}
	}
}
