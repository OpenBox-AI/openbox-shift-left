package telemetry

import (
	"context"
	"errors"
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

// Receiver is the loopback OTLP intake. Hand-rolling it would have made the
// probe's answer load-bearing for whether the lane worked at all.
type Receiver struct {
	cfg     Config
	emitter Emitter
	warn    func(string, ...any)
	logSink io.Writer

	counts signalCounts

	mu        sync.Mutex
	started   bool
	receivers []component.Component
}

// New builds a receiver.
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

// WithEmitter sets the sink every decoded record is handed to. What must not
// happen is that a production path forgets to call this, which is the
// gateway's WithCapture bug; the control test in the command package is what
// holds that.
func WithEmitter(e Emitter) Option { return func(r *Receiver) { r.emitter = e } }

// WithLogWriter redirects the collector's own diagnostics (not this package's
// warnings, which WithWarnFunc owns).
func WithLogWriter(w io.Writer) Option { return func(r *Receiver) { r.logSink = w } }

// WithWarnFunc sets the diagnostic sink. Launchd sends a daemon's stdio to
// /dev/null unless the unit says otherwise, so without a real destination
// these warnings are the difference between a lane that is visibly broken and
// one that is silently recording nothing.
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

// build because it lived inside Start, the only way to reach it was a test
// that could bind, and on a host that cannot bind the whole thing was
// unreachable and looked fine.
func (r *Receiver) build(ctx context.Context) ([]component.Component, error) {
	factory := otlpreceiver.NewFactory()
	cfg, ok := factory.CreateDefaultConfig().(*otlpreceiver.Config)
	if !ok {
		return nil, fmt.Errorf("telemetry: otlpreceiver default config has an unexpected type")
	}

	cfg.Protocols.GRPC = configoptional.None[configgrpc.ServerConfig]()

	httpCfg := confighttp.NewDefaultServerConfig()
	httpCfg.NetAddr.Endpoint = r.cfg.Addr
	httpCfg.MaxRequestBodySize = MaxRequestBodyBytes
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
	// Every receiver's shutdown failure, not just the first: the traces, logs
	// and metrics receivers fail independently and an operator reading one
	// message has no way to know the other two also refused to stop.
	var errs []error
	for _, c := range r.receivers {
		errs = append(errs, c.Shutdown(ctx))
	}
	r.receivers = nil
	r.started = false
	return errors.Join(errs...)
}

// Counts reports how many records have arrived per signal since start. The
// gateway learned that distinction the hard way: it reports alive, configured
// and bypassed, and never "is it capturing", so a perfectly working relay
// recording nothing looked healthy.
func (r *Receiver) Counts() map[Signal]int64 { return r.counts.snapshot() }

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

// receiverSettings every field here must be NON-NIL, and that sentence
// replaces a comment which claimed the opposite.
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

type nopHost struct{}

func (nopHost) GetExtensions() map[component.ID]component.Component { return nil }

// StartStandalone starts the receiver outside a collector pipeline.
func (r *Receiver) StartStandalone(ctx context.Context) error {
	return r.Start(ctx, nopHost{})
}

func (r *Receiver) logWriter() io.Writer {
	if r.logSink != nil {
		return r.logSink
	}
	return os.Stderr
}
