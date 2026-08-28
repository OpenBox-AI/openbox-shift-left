package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
)

// Receiver is the loopback OTLP intake.
//
// The decode is the collector's OWN receiver (D-OSS-2), not a hand-rolled parser.
// That decision buys the thing the wire-format question turned on: otlpreceiver
// accepts BOTH OTLP encodings — protobuf and JSON — on all three signal paths, so
// which encoding this client version happens to emit is a configuration detail
// and not a fork in the design. Hand-rolling it would have made the probe's
// answer load-bearing for whether the lane worked at all.
type Receiver struct {
	cfg     Config
	emitter Emitter
	warn    func(string, ...any)
	logSink io.Writer

	// counts answers doctor's "is it recording", which is a different question
	// from "is it reachable" — a perfectly reachable receiver that no client was
	// ever pointed at is the exact failure OD4 makes a finding.
	counts signalCounts

	mu        sync.Mutex
	started   bool
	receivers []component.Component
}

// New builds a receiver. It does not listen; Start does.
func New(cfg Config, opts ...Option) (*Receiver, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &Receiver{cfg: cfg, warn: func(string, ...any) {}}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Option configures a Receiver.
type Option func(*Receiver)

// WithEmitter sets the sink every decoded record is handed to.
//
// A Receiver without one still listens, decodes and counts — which is why the
// nil case is handled in deliver rather than rejected here: doctor's reachability
// probe and the install-time proof that the port is live both want a receiver
// that answers before any emitter exists. What must not happen is that a
// PRODUCTION path forgets to call this, which is the gateway's WithCapture bug;
// the control test in the command package is what holds that.
func WithEmitter(e Emitter) Option { return func(r *Receiver) { r.emitter = e } }

// WithLogWriter redirects the COLLECTOR's own diagnostics (not this package's
// warnings, which WithWarnFunc owns). Exists so a test can assert that a failing
// receiver says something, rather than trusting that it would.
func WithLogWriter(w io.Writer) Option { return func(r *Receiver) { r.logSink = w } }

// WithWarnFunc sets the diagnostic sink.
//
// launchd sends a daemon's stdio to /dev/null unless the unit says otherwise, so
// without a real destination these warnings are the difference between a lane
// that is visibly broken and one that is silently recording nothing. The unit
// points them at ~/.openbox/telemetry.log for exactly that reason.
func WithWarnFunc(f func(string, ...any)) Option {
	return func(r *Receiver) {
		if f != nil {
			r.warn = f
		}
	}
}

func (r *Receiver) warnf(format string, args ...any) { r.warn(format, args...) }

// Addr is the validated listen address.
func (r *Receiver) Addr() string { return r.cfg.Addr }

// Start builds the OTLP receiver from its factory and begins listening.
func (r *Receiver) Start(ctx context.Context, host component.Host) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("telemetry: receiver already started")
	}

	built, err := r.build(ctx)
	if err != nil {
		return err
	}

	for i, c := range built {
		if err := c.Start(ctx, host); err != nil {
			// Unwind what already started. Leaving a half-started receiver behind
			// would hold the port while the caller believes the install failed —
			// and the install path's next act is to remove the unit, so the port
			// would stay taken by a process nobody is tracking.
			for _, s := range built[:i] {
				_ = s.Shutdown(context.Background())
			}
			return fmt.Errorf("telemetry: start receiver: %w", err)
		}
	}

	r.receivers = built
	r.started = true
	return nil
}

// build constructs the three signal receivers without binding anything.
//
// It is split out of Start for one reason, and it is worth stating: the receiver
// PANICKED the first time it was really started, on a nil *zap.Logger inside the
// factory — and every line of that failure happens HERE, in construction, before
// a socket is involved. Because it lived inside Start, the only way to reach it
// was a test that could bind, and on a host that cannot bind the whole thing was
// unreachable and looked fine. Now the construction half is testable anywhere,
// which is where a "compiles against the real API" claim was quietly standing in
// for "runs".
func (r *Receiver) build(ctx context.Context) ([]component.Component, error) {
	factory := otlpreceiver.NewFactory()
	cfg, ok := factory.CreateDefaultConfig().(*otlpreceiver.Config)
	if !ok {
		return nil, fmt.Errorf("telemetry: otlpreceiver default config has an unexpected type")
	}

	// HTTP only. The gRPC endpoint is switched OFF rather than left at its
	// default: the client exports over HTTP, and a second listener on 4317 would
	// be an additional unauthenticated content endpoint bound for nothing. The
	// smallest surface that carries the traffic is the one to open.
	cfg.Protocols.GRPC = configoptional.None[configgrpc.ServerConfig]()

	httpCfg := confighttp.NewDefaultServerConfig()
	httpCfg.NetAddr.Endpoint = r.cfg.Addr
	// Explicit, not inherited — see MaxRequestBodyBytes for why the library's
	// 20MiB default is wrong for a per-developer loopback daemon.
	httpCfg.MaxRequestBodySize = MaxRequestBodyBytes
	// A read-header timeout the default leaves at zero. Zero means "no deadline",
	// so a client that opens a connection and never completes its headers holds a
	// goroutine indefinitely; on an unauthenticated local listener that is a
	// trivially reachable resource leak.
	httpCfg.ReadHeaderTimeout = 10 * time.Second

	cfg.Protocols.HTTP = configoptional.Some(otlpreceiver.HTTPConfig{
		ServerConfig:   httpCfg,
		TracesURLPath:  "/v1/traces",
		MetricsURLPath: "/v1/metrics",
		LogsURLPath:    "/v1/logs",
	})

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("telemetry: otlp config: %w", err)
	}

	set := receiverSettings(r.logWriter())

	logs, err := consumer.NewLogs(r.consumeLogs)
	if err != nil {
		return nil, fmt.Errorf("telemetry: logs consumer: %w", err)
	}
	traces, err := consumer.NewTraces(r.consumeTraces)
	if err != nil {
		return nil, fmt.Errorf("telemetry: traces consumer: %w", err)
	}
	metrics, err := consumer.NewMetrics(r.consumeMetrics)
	if err != nil {
		return nil, fmt.Errorf("telemetry: metrics consumer: %w", err)
	}

	// All three signals share one underlying listener inside the factory, but
	// each must be created or its URL path 404s. Enabling all three is what keeps
	// the lane quiet in the governed tool's own logs.
	built := make([]component.Component, 0, 3)
	lr, err := factory.CreateLogs(ctx, set, cfg, logs)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create logs receiver: %w", err)
	}
	built = append(built, lr)
	tr, err := factory.CreateTraces(ctx, set, cfg, traces)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create traces receiver: %w", err)
	}
	built = append(built, tr)
	mr, err := factory.CreateMetrics(ctx, set, cfg, metrics)
	if err != nil {
		return nil, fmt.Errorf("telemetry: create metrics receiver: %w", err)
	}
	built = append(built, mr)
	return built, nil
}

// Shutdown stops the receiver. Safe to call when Start never succeeded.
func (r *Receiver) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return nil
	}
	var firstErr error
	for _, c := range r.receivers {
		if err := c.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.receivers = nil
	r.started = false
	return firstErr
}

// Counts reports how many records have arrived per signal since start.
//
// This is what makes doctor able to say RECORDING rather than only REACHABLE.
// The gateway learned that distinction the hard way: it reports alive, configured
// and bypassed, and never "is it capturing", so a perfectly working relay
// recording nothing looked healthy.
func (r *Receiver) Counts() map[Signal]int64 { return r.counts.snapshot() }

// signalCounts is a small per-signal counter set.
type signalCounts struct {
	logs    atomic.Int64
	traces  atomic.Int64
	metrics atomic.Int64
}

func (c *signalCounts) add(s Signal, n int64) {
	switch s {
	case SignalLogs:
		c.logs.Add(n)
	case SignalTraces:
		c.traces.Add(n)
	case SignalMetrics:
		c.metrics.Add(n)
	}
}

func (c *signalCounts) snapshot() map[Signal]int64 {
	return map[Signal]int64{
		SignalLogs:    c.logs.Load(),
		SignalTraces:  c.traces.Load(),
		SignalMetrics: c.metrics.Load(),
	}
}

// receiverSettings builds the component settings the factory needs.
//
// Every field here must be NON-NIL, and that sentence replaces a comment which
// claimed the opposite. It used to pass `component.TelemetrySettings{}` and say
// the collector's telemetry was "left at its no-op defaults" — but the zero value
// of that struct is not a no-op, it is nil interfaces and a nil *zap.Logger, and
// the factory dereferences the logger during creation. The receiver PANICKED on
// the first real Start (otlpreceiver → DropInjectedAttributes → zap clone on nil).
// It compiled, and compiling was never the same as starting.
//
// The collector's own metrics and traces really are discarded, and that part was
// deliberate: this process is a governance sensor, and exporting the observer's
// observations of itself is more surface for nothing any reader here consumes.
// Discarding them is what noop providers do; a nil provider is a crash.
//
// The logger is NOT discarded. It goes to stderr at Warn and above, because this
// daemon's stdio is the only place a silently-not-recording lane can be noticed,
// and a receiver that fails internally after start would otherwise say nothing at
// all.
func receiverSettings(w io.Writer) receiver.Settings {
	encoder := zapcore.NewConsoleEncoder(zap.NewProductionEncoderConfig())
	core := zapcore.NewCore(encoder, zapcore.AddSync(w), zapcore.WarnLevel)
	return receiver.Settings{
		ID: component.NewID(component.MustNewType("otlp")),
		TelemetrySettings: component.TelemetrySettings{
			Logger:         zap.New(core),
			TracerProvider: tracenoop.NewTracerProvider(),
			MeterProvider:  metricnoop.NewMeterProvider(),
		},
		BuildInfo: component.NewDefaultBuildInfo(),
	}
}

// nopHost is the component.Host the standalone daemon supplies.
//
// otlpreceiver's Start takes a host so a collector pipeline can offer it
// extensions — authenticators, and nothing else this receiver uses. Running
// stand-alone there are none, and reporting a fatal error through the host would
// duplicate what Start already returns.
type nopHost struct{}

func (nopHost) GetExtensions() map[component.ID]component.Component { return nil }

// StartStandalone starts the receiver outside a collector pipeline.
//
// It exists so a caller does not have to name a collector type to run this. That
// matters beyond tidiness: the CLI is the only production caller, and every
// collector identifier it mentions is one more place a future dependency bump can
// break the shipping binary rather than this module.
func (r *Receiver) StartStandalone(ctx context.Context) error {
	return r.Start(ctx, nopHost{})
}

// logWriter is where the collector's own diagnostics go.
//
// Defaults to stderr: under launchd that is the file the unit points at, which is
// the only place a failing receiver is visible at all.
func (r *Receiver) logWriter() io.Writer {
	if r.logSink != nil {
		return r.logSink
	}
	return os.Stderr
}
