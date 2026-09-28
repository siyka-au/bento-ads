package bentoads

import (
	"time"

	"cloud.google.com/go/civil"
)

// toBento converts a value as go-ads returns it into Bento's value model.
//
// Bloblang takes Go's numeric types, string, bool, time.Time, []any and
// map[string]any as they are. It does not know time.Duration or the civil
// types, so those become the nearest types it does know, without loss:
//
//	civil.DateTime (DT, LDT)     time.Time in UTC (a Bloblang timestamp)
//	civil.Date (DATE, LDATE)     time.Time at midnight UTC
//	time.Duration (TIME, LTIME)  int64 nanoseconds, as Bloblang's parse_duration gives
//	civil.Time (TOD, LTOD)       int64 nanoseconds since midnight
//
// Structs and arrays are converted member by member. How any of it is rendered
// (RFC 3339, Unix epoch, ...) is left to the pipeline; the data_type metadata
// says which PLC type a value came from.
func toBento(v any) any {
	switch x := v.(type) {
	case civil.DateTime:
		return x.In(time.UTC)
	case civil.Date:
		return x.In(time.UTC)
	case time.Duration:
		return x.Nanoseconds()
	case civil.Time:
		return (time.Duration(x.Hour)*time.Hour + time.Duration(x.Minute)*time.Minute +
			time.Duration(x.Second)*time.Second + time.Duration(x.Nanosecond)).Nanoseconds()
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, m := range x {
			out[k] = toBento(m)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = toBento(m)
		}
		return out
	}
	return v
}
