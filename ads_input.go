// Ported from github.com/RuneRoven/benthosADS (adsPlugin.go, commit 7f7c0e5).
// Original work Copyright (c) 2024 Daniel Helmersson, MIT License. See LICENSE.

package bentoads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	adsLib "github.com/siyka-au/go-ads/v3"
	"github.com/siyka-au/go-ads/v3/ams"
	"github.com/warpstreamlabs/bento/public/service"
)

// bentoLogHandler bridges go-ads's slog-based logging into Bento's logging infrastructure.
type bentoLogHandler struct {
	logger *service.Logger
	level  slog.Level
	attrs  []slog.Attr
}

func (h *bentoLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *bentoLogHandler) Handle(_ context.Context, r slog.Record) error {
	var kvs []any
	for _, a := range h.attrs {
		kvs = append(kvs, a.Key, a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		kvs = append(kvs, a.Key, a.Value.Any())
		return true
	})
	l := h.logger
	if len(kvs) > 0 {
		l = l.With(kvs...)
	}
	switch {
	case r.Level >= slog.LevelError:
		l.Errorf("%s", r.Message)
	case r.Level >= slog.LevelWarn:
		l.Warnf("%s", r.Message)
	case r.Level >= slog.LevelInfo:
		l.Infof("%s", r.Message)
	default:
		l.Debugf("%s", r.Message)
	}
	return nil
}

func (h *bentoLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &bentoLogHandler{logger: h.logger, level: h.level, attrs: append(h.attrs, attrs...)}
}

func (h *bentoLogHandler) WithGroup(_ string) slog.Handler { return h }

func slogLevelFromString(level string) slog.Level {
	switch level {
	case "trace":
		return adsLib.LevelTrace
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.Level(100)
	}
}

type plcSymbol struct {
	name      string
	maxDelay  time.Duration
	cycleTime time.Duration
}

// symbolMeta is what makeMessage needs to label a value, keyed by the symbol
// name exactly as configured (subscribe and read now echo that spelling back
// in Update.Symbol and ReadValues, so no casing normalisation is needed).
type symbolMeta struct {
	dataType string
	baseType string
	size     uint32
	// rangeMin/rangeMax are an IEC 61131-3 subrange's declared bounds (e.g.
	// INT(-10..10)), both nil for a symbol with no subrange restriction. 0 is
	// a legitimate bound, so presence is signaled by non-nil.
	rangeMin, rangeMax *int64
}

// symbolMetaFrom extracts the fields makeMessage needs from a SymbolView.
func symbolMetaFrom(v adsLib.SymbolView) symbolMeta {
	return symbolMeta{
		dataType: v.DataType, baseType: v.BaseTypeName(), size: v.Length,
		rangeMin: v.RangeMin, rangeMax: v.RangeMax,
	}
}

func sanitize(s string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	return re.ReplaceAllString(s, "_")
}

// createSymbolList parses symbol strings into plcSymbol structs.
// Format: "name" or "name:maxDelayMs:cycleTimeMs"
func createSymbolList(s []string, defaultCycleTime int, defaultMaxDelay int) []plcSymbol {
	var result []plcSymbol
	for _, symbol := range s {
		colons := strings.Count(symbol, ":")
		var sym plcSymbol
		if colons != 2 {
			parts := strings.Split(symbol, ":")
			if len(parts) > 0 {
				sym.name = parts[0]
			}
			sym.maxDelay = time.Duration(defaultMaxDelay) * time.Millisecond
			sym.cycleTime = time.Duration(defaultCycleTime) * time.Millisecond
		} else {
			parts := strings.Split(symbol, ":")
			sym.name = parts[0]
			maxDelay, err1 := strconv.Atoi(parts[1])
			if err1 != nil {
				maxDelay = defaultMaxDelay
			}
			cycleTime, err2 := strconv.Atoi(parts[2])
			if err2 != nil {
				cycleTime = defaultCycleTime
			}
			sym.maxDelay = time.Duration(maxDelay) * time.Millisecond
			sym.cycleTime = time.Duration(cycleTime) * time.Millisecond
		}
		result = append(result, sym)
	}
	return result
}

type adsCommInput struct {
	targetIP         string
	targetAMS        string
	targetPort       int
	runtimePort      int
	hostAMS          string
	hostPort         int
	readType         string
	cycleTime        int
	maxDelay         int
	intervalTime     time.Duration
	requestTimeout   time.Duration
	log              *service.Logger
	symbols          []plcSymbol
	notificationChan chan *adsLib.Update
	transmissionMode ams.TransMode

	// mu guards handler. Bento calls Connect and ReadBatch from one goroutine
	// and Close from another, so readers take a snapshot via session() and
	// never touch the field directly.
	mu      sync.Mutex
	handler *adsLib.Session

	// Shutdown signal — closed once by Close() to unblock ReadBatchNotification.
	// Created in the constructor and never reassigned.
	done      chan struct{}
	closeOnce sync.Once

	// Symbol metadata for labelling messages, keyed by symbol name as
	// configured. Filled from SubscribeResult in notification mode; filled
	// lazily via Symbol() on first use in pull mode.
	meta map[string]symbolMeta

	loadSymbols bool
	localMode   bool

	// Route registration settings
	routeUsername    string
	routePassword    string
	routeHostAddress string

	adsLogger *slog.Logger
}

var adsConf = service.NewConfigSpec().
	Summary("Creates an input that reads data from Beckhoff PLCs using ADS protocol. Based on benthosADS by Daniel Helmersson.").
	Description("This input plugin enables Bento to read data directly from Beckhoff PLCs using the ADS protocol. " +
		"Configure the plugin by specifying the PLC's IP address, runtime port, target AMS net ID, etc.").
	Field(service.NewStringField("targetIP").Description("IP address of the Beckhoff PLC.")).
	Field(service.NewStringField("targetAMS").Description("Target AMS net ID.")).
	Field(service.NewIntField("targetPort").Description("TCP port of the PLC ADS gateway.").Default(48898)).
	Field(service.NewIntField("runtimePort").Description("Target runtime port. 851 for TwinCAT 3 (PLC1), 801 for TwinCAT 2.").Default(851)).
	Field(service.NewStringField("hostAMS").Description("Local AMS net ID. 'auto' derives it from the outbound TCP source IP.").Default("auto")).
	Field(service.NewIntField("hostPort").Description("AMS source port used in protocol headers. Any arbitrary value works.").Default(10500)).
	Field(service.NewStringField("readType").Description("Read type, interval or notification (default).").Default("notification")).
	Field(service.NewIntField("maxDelay").Description("Max delay time after value change before PLC should send message, in milliseconds.").Default(100)).
	Field(service.NewIntField("cycleTime").Description("Requested read interval for PLC to scan for changes (notification mode), in milliseconds.").Default(1000)).
	Field(service.NewIntField("intervalTime").Description("Interval between reads in milliseconds for interval read type.").Default(1000)).
	Field(service.NewStringField("logLevel").Description("Log level for ADS connection. Default disabled.").Default("disabled")).
	Field(service.NewIntField("requestTimeout").Description("Timeout for individual ADS requests in milliseconds.").Default(5000)).
	Field(service.NewStringField("transmissionMode").Description("Notification transmission mode: serverOnChange (default), serverCycle, serverOnChange2, serverCycle2.").Default("serverOnChange")).
	Field(service.NewStringField("routeUsername").Description("Username for UDP route registration on the PLC. If set with routePassword, a route will be registered before connecting.").Default("")).
	Field(service.NewStringField("routePassword").Description("Password for UDP route registration on the PLC.").Default("")).
	Field(service.NewStringField("routeHostAddress").Description("The address the PLC should use to reach this client. Auto-detected from outbound connection if empty.").Default("")).
	Field(service.NewBoolField("loadSymbols").Description("Download the full symbol and datatype table from the PLC on connect. Required for struct and array symbols. May cause a brief real-time jitter on the PLC; use with care on large programs.").Default(false)).
	Field(service.NewBoolField("localMode").Description("Connect through the TwinCAT router on this machine (127.0.0.1) and let it assign the local AMS address, instead of using a route. Use for a runtime on the same host, e.g. a usermode runtime; targetAMS is still the runtime's own NetID.").Default(false).Advanced()).
	Field(service.NewStringListField("symbols").Description("Symbols to read. Format: 'MAIN.var' or 'MAIN.var:maxDelayMs:cycleTimeMs'. " +
		"Examples: 'MAIN.counter', '.globalCounter', 'MAIN.var:50:100'"))

func newAdsCommInput(conf *service.ParsedConfig, mgr *service.Resources) (service.BatchInput, error) {
	m, err := adsCommInputFromConfig(conf, mgr)
	if err != nil {
		return nil, err
	}
	return service.AutoRetryNacksBatched(m), nil
}

func adsCommInputFromConfig(conf *service.ParsedConfig, mgr *service.Resources) (*adsCommInput, error) {
	logLevel, err := conf.FieldString("logLevel")
	if err != nil {
		return nil, err
	}
	adsLogger := slog.New(&bentoLogHandler{
		logger: mgr.Logger(),
		level:  slogLevelFromString(logLevel),
	})

	targetIP, err := conf.FieldString("targetIP")
	if err != nil {
		return nil, err
	}

	targetAMS, err := conf.FieldString("targetAMS")
	if err != nil {
		return nil, err
	}

	if addr, perr := netip.ParseAddr(targetIP); perr != nil || !addr.Is4() {
		return nil, fmt.Errorf("targetIP: %q is not a valid IPv4 address", targetIP)
	}
	if _, err = ams.ParseNetID(targetAMS); err != nil {
		return nil, fmt.Errorf("targetAMS: %w", err)
	}

	targetPort, err := conf.FieldInt("targetPort")
	if err != nil {
		return nil, err
	}

	runtimePort, err := conf.FieldInt("runtimePort")
	if err != nil {
		return nil, err
	}
	if runtimePort < 0 || runtimePort > 65535 {
		return nil, fmt.Errorf("runtimePort %d out of range 0–65535", runtimePort)
	}

	hostAMS, err := conf.FieldString("hostAMS")
	if err != nil {
		return nil, err
	}
	if hostAMS != "auto" && hostAMS != "" {
		if _, err = ams.ParseNetID(hostAMS); err != nil {
			return nil, fmt.Errorf("hostAMS: %w", err)
		}
	}

	hostPort, err := conf.FieldInt("hostPort")
	if err != nil {
		return nil, err
	}
	if hostPort < 0 || hostPort > 65535 {
		return nil, fmt.Errorf("hostPort %d out of range 0–65535", hostPort)
	}

	readType, err := conf.FieldString("readType")
	if err != nil {
		return nil, err
	}
	if readType != "notification" && readType != "interval" {
		return nil, errors.New("readType must be 'notification' or 'interval'")
	}

	maxDelay, err := conf.FieldInt("maxDelay")
	if err != nil {
		return nil, err
	}

	cycleTime, err := conf.FieldInt("cycleTime")
	if err != nil {
		return nil, err
	}

	symbols, err := conf.FieldStringList("symbols")
	if err != nil {
		return nil, err
	}

	intervalTimeInt, err := conf.FieldInt("intervalTime")
	if err != nil {
		return nil, err
	}

	requestTimeoutInt, err := conf.FieldInt("requestTimeout")
	if err != nil {
		return nil, err
	}

	transmissionModeStr, err := conf.FieldString("transmissionMode")
	if err != nil {
		return nil, err
	}
	var transmissionMode ams.TransMode
	if err = transmissionMode.UnmarshalText([]byte(transmissionModeStr)); err != nil {
		return nil, fmt.Errorf("transmissionMode: %w", err)
	}

	routeUsername, err := conf.FieldString("routeUsername")
	if err != nil {
		return nil, err
	}

	routePassword, err := conf.FieldString("routePassword")
	if err != nil {
		return nil, err
	}

	routeHostAddress, err := conf.FieldString("routeHostAddress")
	if err != nil {
		return nil, err
	}

	loadSymbols, err := conf.FieldBool("loadSymbols")
	if err != nil {
		return nil, err
	}

	localMode, err := conf.FieldBool("localMode")
	if err != nil {
		return nil, err
	}

	symbolList := createSymbolList(symbols, cycleTime, maxDelay)
	m := &adsCommInput{
		targetIP:         targetIP,
		targetAMS:        targetAMS,
		targetPort:       targetPort,
		runtimePort:      runtimePort,
		hostAMS:          hostAMS,
		hostPort:         hostPort,
		readType:         readType,
		maxDelay:         maxDelay,
		cycleTime:        cycleTime,
		symbols:          symbolList,
		log:              mgr.Logger(),
		intervalTime:     time.Duration(intervalTimeInt) * time.Millisecond,
		requestTimeout:   time.Duration(requestTimeoutInt) * time.Millisecond,
		notificationChan: make(chan *adsLib.Update, 4096),
		done:             make(chan struct{}),
		transmissionMode: transmissionMode,
		loadSymbols:      loadSymbols,
		localMode:        localMode,
		routeUsername:    routeUsername,
		routePassword:    routePassword,
		routeHostAddress: routeHostAddress,
		adsLogger:        adsLogger,
	}

	return m, nil
}

func init() {
	err := service.RegisterBatchInput(
		"ads", adsConf,
		func(conf *service.ParsedConfig, mgr *service.Resources) (service.BatchInput, error) {
			return newAdsCommInput(conf, mgr)
		})
	if err != nil {
		panic(err)
	}
}

// session returns the current handler, or nil when not connected.
func (g *adsCommInput) session() *adsLib.Session {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.handler
}

// dropSession clears handler if it is still sess and closes sess in the
// background. It is a no-op if Close or a reconnect already replaced it.
func (g *adsCommInput) dropSession(sess *adsLib.Session) {
	g.mu.Lock()
	if g.handler == sess {
		g.handler = nil
	}
	g.mu.Unlock()
	go func() { _ = sess.Close() }()
}

func (g *adsCommInput) closed() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

func (g *adsCommInput) Connect(ctx context.Context) error {
	if g.closed() {
		return service.ErrEndOfInput
	}
	if g.session() != nil {
		return nil
	}

	g.log.Infof("Creating new connection")

	var connOpts []adsLib.Option
	if g.adsLogger != nil {
		connOpts = append(connOpts, adsLib.WithLogger(g.adsLogger))
	}
	if g.localMode {
		connOpts = append(connOpts, adsLib.WithLocalMode())
	}

	if g.routeUsername != "" && g.routePassword != "" {
		// An empty route name makes go-ads name the route after the callback
		// IP itself (its own outbound source IP, or routeHostAddress below).
		g.log.Infof("Route will be registered on PLC %s", g.targetIP)
		connOpts = append(connOpts, adsLib.WithRoute("", g.routeUsername, g.routePassword))
		if g.routeHostAddress != "" {
			connOpts = append(connOpts, adsLib.WithHostIP(g.routeHostAddress))
		}
	}

	target, err := ams.NewAddress(g.targetAMS, ams.Port(g.runtimePort))
	if err != nil {
		g.log.Errorf("Invalid target AMS %q: %v", g.targetAMS, err)
		return err
	}

	// The local Address is always set so hostPort takes effect; NetID stays
	// zero (go-ads then derives it from the outbound TCP source IP) unless
	// hostAMS was explicitly configured.
	local := ams.Address{Port: ams.Port(g.hostPort)}
	if g.hostAMS != "" && g.hostAMS != "auto" {
		local, err = ams.NewAddress(g.hostAMS, ams.Port(g.hostPort))
		if err != nil {
			g.log.Errorf("Invalid local AMS %q: %v", g.hostAMS, err)
			return err
		}
	}
	connOpts = append(connOpts, adsLib.WithLocalAddress(local))

	if g.requestTimeout > 0 {
		connOpts = append(connOpts, adsLib.WithRequestTimeout(g.requestTimeout))
	}

	// Use Background ctx for session lifetime — Bento passes a per-call ctx to Connect
	// that would tear the session down as soon as Connect returns. Teardown is driven by Close().
	sess, err := adsLib.NewSession(context.Background(), adsLib.Endpoint{
		Host:   g.targetIP,
		Port:   g.targetPort,
		Target: target,
	}, connOpts...)
	if err != nil {
		g.log.Errorf("Failed to create session: %v", err)
		return err
	}

	success := false
	defer func() {
		if !success {
			_ = sess.Close()
		}
	}()

	g.log.Infof("Connecting to PLC")
	if err = sess.Connect(ctx); err != nil {
		g.log.Errorf("Failed to connect to PLC at %s: %v", g.targetIP, err)
		return err
	}

	g.meta = make(map[string]symbolMeta, len(g.symbols))

	if g.loadSymbols {
		g.log.Infof("Loading symbol and datatype table from PLC (loadSymbols=true)")
		if err = sess.LoadSymbols(ctx); err != nil {
			g.log.Errorf("LoadSymbols failed: %v", err)
			return err
		}
		g.log.Infof("Symbol table loaded")
	}

	if g.readType == "notification" {
		configs := make([]adsLib.NotificationConfig, len(g.symbols))
		for i, symbol := range g.symbols {
			configs[i] = adsLib.NotificationConfig{
				Symbol:    symbol.name,
				MaxDelay:  symbol.maxDelay,
				CycleTime: symbol.cycleTime,
				Mode:      g.transmissionMode,
			}
		}

		results, err := sess.SubscribeAll(ctx, configs, g.notificationChan)
		if err != nil {
			g.log.Errorf("Batch subscribe failed: %v", err)
			return err
		}

		// The result carries each symbol's metadata alongside the outcome, so
		// no follow-up Symbol() round-trip is needed to label its messages.
		registered := 0
		for i, r := range results {
			if r.Err != nil {
				g.log.Errorf("Notification symbol %q failed: %v", configs[i].Symbol, r.Err)
				continue
			}
			registered++
			g.meta[configs[i].Symbol] = symbolMetaFrom(r.Symbol)
		}
		if registered == 0 && len(configs) > 0 {
			return fmt.Errorf("no symbols registered for notifications (%d symbols all failed to resolve)", len(configs))
		}
		g.log.Infof("Registered %d/%d notification symbols", registered, len(configs))

		// TwinCAT sends each symbol's current value on subscribe; it stays in the
		// channel and becomes the first ReadBatch, so the pipeline starts from the
		// PLC's actual state.
	}

	// Publish under the lock, re-checking done so a Close that ran while we were
	// connecting can't miss this session and leak it.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed() {
		return service.ErrEndOfInput
	}
	g.handler = sess
	success = true
	return nil
}

// makeMessage builds the message for one symbol's value: the value converted to
// Bento's types as structured content, and the symbol's name and type as
// metadata.
func (g *adsCommInput) makeMessage(symbol string, value any) *service.Message {
	msg := service.NewMessage(nil)
	msg.SetStructuredMut(toBento(value))
	msg.MetaSet("symbol_name", sanitize(symbol))
	if m, ok := g.meta[symbol]; ok {
		msg.MetaSet("data_type", m.dataType)
		msg.MetaSet("data_size", strconv.FormatUint(uint64(m.size), 10))
		if m.baseType != "" {
			msg.MetaSet("base_type", m.baseType)
		}
		if m.rangeMin != nil {
			msg.MetaSet("range_min", strconv.FormatInt(*m.rangeMin, 10))
		}
		if m.rangeMax != nil {
			msg.MetaSet("range_max", strconv.FormatInt(*m.rangeMax, 10))
		}
	}
	return msg
}

func (g *adsCommInput) ReadBatchPull(ctx context.Context) (service.MessageBatch, service.AckFunc, error) {
	g.log.Debugf("ReadBatchPull called")
	start := time.Now()
	sess := g.session()
	if sess == nil {
		return nil, nil, service.ErrNotConnected
	}

	names := make([]string, len(g.symbols))
	for i, symbol := range g.symbols {
		names[i] = symbol.name
	}

	// A *BatchError names the symbols that failed and leaves the rest in values;
	// any other error means the read as a whole failed.
	values, err := sess.ReadValues(ctx, names)
	var batchErr *adsLib.BatchError
	switch {
	case err == nil:
	case errors.As(err, &batchErr):
		for _, item := range batchErr.Items {
			g.log.Warnf("Read failed: %v", item)
		}
	default:
		if g.closed() {
			return nil, nil, service.ErrEndOfInput
		}
		select {
		case <-sess.Done():
			g.log.Warnf("Session ended: %v", sess.Err())
			g.dropSession(sess)
			return nil, nil, service.ErrNotConnected
		default:
		}
		g.log.Warnf("Batch read failed (will retry): %v", err)
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		return service.MessageBatch{}, func(_ context.Context, _ error) error { return nil }, nil
	}

	// Lazily populate type metadata on first use (a single-symbol lookup, no
	// extra round-trip once cached).
	for _, sym := range g.symbols {
		if _, ok := g.meta[sym.name]; !ok {
			if view, viewErr := sess.Symbol(ctx, sym.name); viewErr == nil {
				g.meta[sym.name] = symbolMetaFrom(view)
			}
		}
	}

	msgs := make(service.MessageBatch, 0, len(values))
	for _, symbol := range g.symbols {
		if val, ok := values[symbol.name]; ok {
			msgs = append(msgs, g.makeMessage(symbol.name, val))
		}
	}

	if remaining := g.intervalTime - time.Since(start); remaining > 0 {
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return msgs, func(_ context.Context, _ error) error { return nil }, nil
}

func (g *adsCommInput) ReadBatchNotification(ctx context.Context) (service.MessageBatch, service.AckFunc, error) {
	g.log.Debugf("ReadBatchNotification called")

	// Short-lived context so ReadBatch returns periodically even with slow-changing symbols.
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// A nil session's Done channel is nil and never fires.
	sess := g.session()
	var sessDone <-chan struct{}
	if sess != nil {
		sessDone = sess.Done()
	}

	var first *adsLib.Update
	select {
	case first = <-g.notificationChan:
		if first == nil {
			g.log.Warnf("Received nil update from ADS library, skipping")
			return nil, func(_ context.Context, _ error) error { return nil }, nil
		}
	case <-g.done:
		return nil, nil, service.ErrEndOfInput
	case <-sessDone:
		g.log.Warnf("Session ended: %v", sess.Err())
		g.dropSession(sess)
		return nil, nil, service.ErrNotConnected
	case <-waitCtx.Done():
		return nil, func(_ context.Context, _ error) error { return nil }, nil
	}

	msgs := service.MessageBatch{g.makeMessage(first.Symbol, first.Value)}

	// Drain all pending notifications without blocking to keep the channel buffer available.
	for {
		select {
		case update := <-g.notificationChan:
			if update != nil {
				msgs = append(msgs, g.makeMessage(update.Symbol, update.Value))
			}
		default:
			return msgs, func(_ context.Context, _ error) error { return nil }, nil
		}
	}
}

func (g *adsCommInput) ReadBatch(ctx context.Context) (service.MessageBatch, service.AckFunc, error) {
	g.log.Debugf("ReadBatch called")
	if g.readType == "notification" {
		return g.ReadBatchNotification(ctx)
	}
	return g.ReadBatchPull(ctx)
}

// Close shuts down the ADS connection.
//
//nolint:revive
func (g *adsCommInput) Close(ctx context.Context) error {
	g.log.Debugf("Close called")
	g.closeOnce.Do(func() { close(g.done) })

	g.mu.Lock()
	sess := g.handler
	g.handler = nil
	g.mu.Unlock()

	if sess != nil {
		g.log.Infof("Closing down, cleaning up PLC handles")
		if cerr := sess.Close(); cerr != nil {
			g.log.Warnf("Handler close error: %v", cerr)
		}
	}
	return nil
}
