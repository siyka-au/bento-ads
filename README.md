# bento-ads

A [Bento](https://github.com/warpstreamlabs/bento) input plugin that reads data from
Beckhoff PLCs over ADS, built on [go-ads](https://github.com/siyka-au/go-ads)
(`github.com/siyka-au/go-ads/v3`).

Started as a port of [benthosADS](https://github.com/RuneRoven/benthosADS) by
Daniel Helmersson (MIT), moved from the Benthos/Redpanda plugin API to Bento's
`github.com/warpstreamlabs/bento/public/service`.

## Build

```sh
go build -o bento-ads ./cmd/bento-ads
cp .env.example .env   # then set your PLC's IP, NetID and port
./bento-ads -e .env -c example/config.yaml
```

The example configs read the connection from `ADS_TARGET_IP`, `ADS_TARGET_NET_ID`,
`ADS_TARGET_PORT` and `ADS_LOCAL_MODE`. `.env` is gitignored, so machine-specific
values stay out of the repo.

## Tests

```sh
go test .                          # unit tests, no PLC needed
go test -run Integration -v .      # against a PLC, using .env
```

The integration tests expect the AdsGo_Testing PLC project
(`siyka/ads-go/plc/testing`) and are skipped when `ADS_TARGET_NET_ID` is unset.

`cmd/bento-ads` bundles every standard Bento component plus the `ads` input. To add
the input to your own Bento build, blank-import the package:

```go
import _ "github.com/siyka-au/bento-ads"
```

go-ads is taken from the local fork at `../go-ads` through a `replace` directive in
`go.mod`. Change or drop that line to use a published version.

## Configuration

See `example/config.yaml`. The fields are:

| Field | Default | Description |
|---|---|---|
| `targetIP` | | IP address of the PLC |
| `targetAMS` | | AMS NetID of the PLC |
| `targetPort` | `48898` | TCP port of the PLC ADS gateway |
| `runtimePort` | `851` | ADS port of the runtime (851 = TwinCAT 3, 801 = TwinCAT 2) |
| `hostAMS` | `auto` | Local AMS NetID; `auto` derives it from the outbound source IP |
| `hostPort` | `10500` | Local AMS port |
| `readType` | `notification` | `notification` or `interval` |
| `maxDelay` | `100` | Notification max delay (ms) |
| `cycleTime` | `1000` | Notification cycle time (ms) |
| `intervalTime` | `1000` | Poll interval for `interval` mode (ms) |
| `requestTimeout` | `5000` | Per-request timeout (ms) |
| `transmissionMode` | `serverOnChange` | `serverOnChange`, `serverCycle`, `serverOnChange2`, `serverCycle2` |
| `routeUsername` / `routePassword` | | If both are set, a route is registered on the PLC before connecting. It is named after the address it points at |
| `routeHostAddress` | | The address the PLC uses to reach this host (auto-detected if empty) |
| `localMode` | `false` | Connect through the local TwinCAT router (runtime on this machine) |
| `loadSymbols` | `false` | Download the full symbol and datatype table on connect (needed for structs and arrays) |
| `logLevel` | `disabled` | go-ads log level: `trace`, `debug`, `info`, `warn`, `error` |
| `symbols` | | `MAIN.var` or `MAIN.var:maxDelayMs:cycleTimeMs` |

## Messages

Each message is one symbol's value as structured content, with metadata
`symbol_name`, `data_type` (as TwinCAT reports it), `base_type` and `data_size`.

go-ads returns values as Go types; the input converts them to Bento's own types
and does no further formatting:

| PLC type | Message value |
|---|---|
| `BOOL`, integers, `REAL`/`LREAL`, `STRING`/`WSTRING` | the Go type as is (`bool`, `int16`, `uint32`, `float32`, `string`, ...) |
| `DT`, `LDT` | timestamp (`time.Time`, UTC) |
| `DATE`, `LDATE` | timestamp at midnight UTC |
| `TIME`, `LTIME` | integer nanoseconds, as Bloblang's `parse_duration` gives |
| `TOD`, `LTOD` | integer nanoseconds since midnight |
| struct | object keyed by member name |
| array | array in index order, nested per dimension |

How a value is rendered is left to the pipeline, for example:

```yaml
pipeline:
  processors:
    - mapping: |
        root.value = match {
          meta("data_type") == "DATE_AND_TIME" => this.ts_format("2006-01-02T15:04:05Z07:00"),
          meta("data_type") == "TIME" => this / 1000000,   # milliseconds
          _ => this,
        }
```

Use `match` without an expression, as here: `match meta("data_type") { ... }`
rebinds `this` to the data type string.

A timestamp left as is is written as RFC 3339 when the message is serialised.

## License

MIT. See `LICENSE`.
