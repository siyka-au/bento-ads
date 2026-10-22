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

## Running as a service

`bento-ads` can install and manage itself as an OS service, via
[kardianos/service](https://github.com/kardianos/service): a real Windows
service, a systemd/Upstart/SysV/OpenRC service on Linux, or a FreeBSD rc.d
service -- not a third-party wrapper, so stopping it runs Bento's own
graceful shutdown rather than a hard kill. OpenBSD and NetBSD aren't
supported by the underlying library.

```sh
bento-ads service install -c C:\path\to\config.yaml -e C:\path\to\.env
bento-ads service start
bento-ads service stop
bento-ads service restart
bento-ads service uninstall
```

`-c`/`-e` are only needed (and only read) at `install` time -- they're
resolved to absolute paths and baked into the service definition, since the
OS starts the service from a working directory you don't control. The other
actions just operate on the already-installed service.

Installing a service generally needs elevated privileges (an Administrator
shell on Windows, `sudo` on Linux/FreeBSD).

Running `bento-ads` directly -- interactively, or as the entrypoint of a
container -- is unaffected and behaves exactly as in [Build](#build) above;
the service machinery only activates for the `service` subcommand and when
actually launched by the OS service manager.

When launched by the OS service manager, service mode now runs through
exactly the same startup path as an interactive run -- Bento's own CLI,
given the same `-c`/`-e` flags baked in at install time -- rather than a
separate, narrower code path, so a config that relies on CLI-only sections
(`telemetry_resources`, `error_handling`, `logger.file`, metrics/tracer
resources, etc.) works identically whichever way `bento-ads` was started.

Service mode writes to the OS's system log -- the Windows Event Log
(Application log, source `bento-ads`) on Windows, syslog/journald on Linux --
but only what would otherwise be lost, since that log is shared with every
other program on the machine and capped at a fixed size (20 MB by default on
Windows); a chatty `DEBUG` config can fill that in well under an hour and
evict everyone else's events with it. Specifically:

* Anything that fails before Bento's own logger exists (a bad `-c`/`-e`
  path, a config that fails to parse or lint, ...) always goes to the
  system log, at `ERROR` -- a service manager discards stdout/stderr, so
  without this a broken install would have nowhere to report to at all.
* `bento-ads` itself logs its own service lifecycle directly to the system
  log, a handful of lines per start/stop: starting (with the `-c`/`-e`
  paths and build version), stopping, stopped, and a graceful-shutdown
  timeout or an unrequested exit, both at `ERROR`.
* Everything Bento's own configured `logger` emits is sent to the system
  log too, filtered by `logger.level` as usual, but **only when the config
  has no `logger.file` path set**. With `logger.file` configured, every one
  of those records is already being kept on disk (and, in most setups,
  shipped on to Grafana/Loki from there), so the system log doesn't need a
  second copy -- it would only drown out everyone else's events. Without
  `logger.file`, a service's stdout is discarded and nothing else keeps
  those records, so they go to the system log instead. A `DEBUG`-level line
  only reaches the system log once `logger.level` is `DEBUG` (or `TRACE`,
  which the system log can't distinguish from `DEBUG`), same as always.

Known gap: if `logger.file` is configured but fails at runtime (e.g. the
disk fills up), that failure isn't detected or mirrored to the system log --
the log file is assumed to work once it's configured.

## Tests

```sh
go test .                          # unit tests, no PLC needed
go test -run Integration -v .      # against a PLC, using .env
```

The integration tests expect the `AdsClient_DeterministicTester` PLC project,
part of the [ads-client-tester](https://github.com/siyka-au/ads-client-tester)
TwinCAT solution, and are skipped when `ADS_TARGET_NET_ID` is unset.

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
| `enums` | `number` | How an enum value renders in the message body: `number` (its declared integer value, unchanged from before go-ads decoded enums at all), `name` (its declared constant name, or the number for an undeclared value), or `both` (`{"name": ..., "value": ...}`, `name` omitted when unknown) |
| `logLevel` | `warn` | go-ads log level: `trace`, `debug`, `info`, `warn`, `error`, `disabled`. `disabled` drops everything, including reconnect failures, the PLC leaving RUN and heartbeat loss; records are also subject to Bento's own `logger.level` |
| `symbols` | | `MAIN.var` or `MAIN.var:maxDelayMs:cycleTimeMs` |

## Metrics

Alongside Bento's own component metrics (`input_ads`, `input_latency_ns`, ...),
the `ads` input emits:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ads_disconnects` | counter | | Incremented each time go-ads detects the TCP connection has dropped |
| `ads_reconnects` | counter | | Incremented each time go-ads successfully reconnects after a drop |
| `ads_connected` | gauge | | `1` while the transport is up (since `Connect` or the last reconnect), `0` after a disconnect or `Close`. A direct "is it down right now" signal, where the two counters above only say how often it has changed |
| `ads_reconnect_failures` | counter | | Incremented after each reconnect-loop iteration that fails (go-ads's `WithOnReconnectAttempt`). The attempt deferred only because the PLC runtime is not in RUN yet does not count, matching go-ads not charging it against the attempt budget either |
| `ads_reads` | counter | `result` = `ok`\|`partial`\|`error` | One increment per interval read (`readType: interval`). `partial` is a `*adsLib.BatchError` (the PLC answered per-item and at least one item failed); `error` is everything else, including a transport failure such as a router rejection or a timed-out `SumRead` |
| `ads_read_errors` | counter | `reason` = `timeout`\|`router`\|`device`\|`canceled`\|`other` | Only incremented alongside `ads_reads{result="error"}`; classifies *why*. See [Read error classification](#read-error-classification) below |
| `ads_symbols_failed` | counter | | Number of symbols named in a partial batch's failed items (`ads_reads{result="partial"}`) |
| `ads_last_read_success` | gauge | | Unix seconds of the last read (`interval` mode) or drained notification (`notification` mode) that produced at least one value. A staleness alert (see below) is the main reason this exists |
| `ads_read_latency_ns` | timer | | The PLC round trip for one interval read (`readType: interval`), in nanoseconds -- just the read, unlike `input_latency_ns`'s read-to-acknowledge |
| `ads_plc_running` | gauge | | `1` while the PLC runtime is known to be in RUN, `0` otherwise (including "not known yet") |
| `ads_plc_state` | gauge | | The numeric `ams.State` last observed -- see [PLC runtime states](#plc-runtime-states) below for what each value means |
| `ads_symbol_version_changes` | counter | | Incremented on each online-change detection (go-ads's `WithOnSymbolVersionChanged`) |
| `ads_notifications_dropped` | counter | | Incremented each time a notification is dropped because this plugin's channel was full (go-ads's `WithOnNotificationDropped`). No per-symbol label -- see the field's doc comment in `ads_input.go` |
| `ads_heartbeat_silences` | counter | | Incremented each time go-ads's internal subscription heartbeat (`notification` mode) goes silent long enough to be treated as evidence the subscriptions died (go-ads's `WithOnHeartbeatSilent`) |

go-ads already logs the disconnect/reconnect/reconnect-attempt/runtime-state/
symbol-version/notification-dropped/heartbeat-silent events itself (subject to
`logLevel`), so the metrics above add counts and gauges only, not duplicate
log lines.

With the fork's OTel metrics exporter, a counter named `ads_foo` above appears
in Prometheus as `ads_foo_total`.

Example alert expressions:

```promql
# No data for about two minutes, in either read mode. Metrics are pushed
# periodically (push_interval, e.g. 60s), so this value lags by up to one
# interval even when reads are healthy: keep the threshold above it.
time() - ads_last_read_success > 120

# The transport is down right now.
ads_connected == 0

# Reads are failing at more than 0 per second, broken down by cause.
sum by (reason) (rate(ads_read_errors_total[5m])) > 0

# The PLC itself has left RUN (CONFIG, STOP, a restart, ...).
ads_plc_running == 0
```

### Read error classification

`ads_read_errors`'s `reason` label is deliberately a small, fixed set so the
metric's cardinality never grows and a dashboard built against it never needs
to change. `classifyReadError` (in `ads_metrics.go`) maps the error
`ReadBatchPull` got from `ReadValues` using `errors.Is`/`errors.As` against
go-ads/`ams` error types, never by matching message text:

| `reason` | Matches |
|---|---|
| `timeout` | `context.DeadlineExceeded`, or an ADS-level timeout return code (`ams.ReturnCodeDeviceTimeout`, `ReturnCodeClientSyncTimeout`, `ReturnCodeWsaeTimedOut`) |
| `router` | `ams.RouterError` -- a rejection from the AMS router itself, never a per-item PLC verdict. This is the incident that motivated this metric: `SumRead failed: context deadline exceeded` followed by `AMS router: 0x001A: TCP send error`, with go-ads never declaring a disconnect |
| `device` | Any other `ams.ReturnCode` -- the PLC answered with a per-item ADS error |
| `canceled` | `context.Canceled` (e.g. shutdown) |
| `other` | Everything else, including go-ads's own `ErrDisconnected`/`ErrTransportClosed`/`ErrRuntimeNotRunning` |

`ams.RouterError` is checked before any `ams.ReturnCode` check: go-ads's own
`RouterError.Is` deliberately does not unwrap to `ReturnCode`, precisely so a
router-level rejection is never read as a verdict about the symbols requested
-- a `RouterError` is classified `router` however it is wrapped, regardless of
which code it happens to carry.

### PLC runtime states

`ads_plc_state`'s value is go-ads' `ams.State`, the device's own ADS state
code (from `ams/state.go`):

| Value | State | Value | State |
|---|---|---|---|
| 0 | Invalid (not known yet) | 9 | PowerFailure |
| 1 | Idle | 10 | PowerGood |
| 2 | Reset | 11 | Error |
| 3 | Init | 12 | Shutdown |
| 4 | Start | 13 | Suspend |
| 5 | Run | 14 | Resume |
| 6 | Stop | 15 | Config |
| 7 | SaveCfg | 16 | Reconfig |
| 8 | LoadCfg | 255 | MaxStates |

Only `Run` (5) counts as "running" for `ads_plc_running`. `Config` (15) and
`Reconfig` (16) are the two states go-ads itself treats as provably not
serving a runtime port; everything else (including `Stop`) is permitted
rather than refused, since go-ads has only measured those two in the field --
see go-ads' `runtimeDefinitelyNotServing`.

### Tracing

In `interval` mode, each PLC read gets its own `ads.read` span, started before
the read and ended as soon as it returns -- before Bento's own `input_ads`
span exists, so `input_ads` is created as `ads.read`'s child (one trace per
read: `ads.read` -> `input_ads` -> processors -> output). `notification` mode
has no discrete request to time or trace, so it gets no `ads.read` span.

`ads.read`'s attributes:

| Attribute | Meaning |
|---|---|
| `ads.target.ams_net_id` | `targetAMS` |
| `ads.target.ip` | `targetIP` |
| `ads.target.port` | `runtimePort` |
| `ads.symbols.requested` | Number of symbols asked for |
| `ads.symbols.returned` | Number of symbols that came back with a value |
| `ads.symbols.failed` | Only set on a partial batch error: number of symbols that failed |

A partial batch error (some symbols failed, others still returned a value) is
recorded on the span but does not mark it failed -- some data beats none. The
span is only marked failed when the read fails outright, or a batch error
leaves no symbols with a value at all.

## Messages

Each message is one symbol's value as structured content, with metadata
`symbol_name`, `data_type` (as TwinCAT reports it), `base_type`, `data_size` and
`timestamp`. `timestamp` is when the value was sampled, as RFC 3339 with
nanoseconds: the PLC's own time for notifications, and the start of the read for
interval reads. It is metadata, so the body is unchanged. To put it in the
payload, map it in the pipeline, e.g. `root.timestamp = meta("timestamp")`.

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
