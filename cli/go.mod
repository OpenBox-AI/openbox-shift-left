module github.com/openbox-ai/openbox-shift-left/cli

go 1.27.0

require (
	github.com/google/renameio/v2 v2.0.2
	github.com/kardianos/service v1.3.0
	// SL4-WIRE-1: the CLI registers the real Claude Code installer and depends
	// on the shared install-time SPI both it and the adapter implement.
	github.com/openbox-ai/openbox-shift-left/adapters/claude-code v0.0.0
	// STORY-SL7-A: the real Codex installer + hook engine.
	github.com/openbox-ai/openbox-shift-left/adapters/codex v0.0.0
	github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig v0.0.0
	github.com/openbox-ai/openbox-shift-left/adapters/common/git v0.0.0
	github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow v0.0.0
	github.com/openbox-ai/openbox-shift-left/client v0.0.0
	// The local OpenBox gateway (ADR-0021). The CLI serves it; the gateway module
	// imports nothing from here, and imports no adapter at all.
	github.com/openbox-ai/openbox-shift-left/gateway v0.0.0
	github.com/openbox-ai/openbox-shift-left/provider v0.0.0
	github.com/openbox-ai/openbox-shift-left/telemetry v0.0.0
	// Masked credential input and TTY detection that works on native Windows,
	// where the stdlib mode check misjudges a console handle (golang/go#23123).
	// ADR-0015 took this module's first external dependency for it.
	golang.org/x/term v0.45.0
)

require (
	dario.cat/mergo v1.0.1 // indirect
	github.com/BobuSumisu/aho-corasick v1.0.3 // indirect
	github.com/Masterminds/goutils v1.1.1 // indirect
	github.com/Masterminds/semver/v3 v3.3.0 // indirect
	github.com/Masterminds/sprig/v3 v3.3.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/STARRY-S/zip v0.2.1 // indirect
	github.com/andybalholm/brotli v1.1.2-0.20250424173009-453214e765f3 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/bodgit/plumbing v1.3.0 // indirect
	github.com/bodgit/sevenzip v1.6.0 // indirect
	github.com/bodgit/windows v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/lipgloss v0.5.0 // indirect
	github.com/dsnet/compress v0.0.2-0.20230904184137-39efe44ab707 // indirect
	github.com/fatih/semgroup v1.2.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/foxboron/go-tpm-keyfiles v0.0.0-20251226215517-609e4778396f // indirect
	github.com/fsnotify/fsnotify v1.8.0 // indirect
	github.com/gitleaks/go-gitdiff v0.9.1 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/golang/snappy v1.0.0 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/h2non/filetype v1.1.3 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/hashicorp/hcl v1.0.0 // indirect
	github.com/huandu/xstrings v1.5.0 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/pgzip v1.2.6 // indirect
	github.com/knadh/koanf/maps v0.1.3 // indirect
	github.com/knadh/koanf/providers/confmap v1.0.1 // indirect
	github.com/knadh/koanf/v2 v2.3.6 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/magiconair/properties v1.8.9 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.14 // indirect
	github.com/mholt/archives v0.1.2 // indirect
	github.com/minio/minlz v1.0.0 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/muesli/reflow v0.2.1-0.20210115123740-9e1d0d53df68 // indirect
	github.com/muesli/termenv v0.15.1 // indirect
	github.com/nwaples/rardecode/v2 v2.1.0 // indirect
	// The in-process decision engine the enforce hook evaluates against (ADR-0006
	// retired the socket sidecar and its `sidecar serve` subcommand; cli imports
	// decision, never the reverse).
	github.com/openbox-ai/openbox-shift-left/decision v0.0.0 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/pierrec/lz4/v4 v4.1.28 // indirect
	github.com/rivo/uniseg v0.2.0 // indirect
	github.com/rs/cors v1.11.1 // indirect
	github.com/rs/zerolog v1.33.0 // indirect
	github.com/sagikazarmark/locafero v0.7.0 // indirect
	github.com/sagikazarmark/slog-shim v0.1.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/sorairolake/lzip-go v0.3.5 // indirect
	github.com/sourcegraph/conc v0.3.0 // indirect
	github.com/spf13/afero v1.12.0 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/spf13/pflag v1.0.6 // indirect
	github.com/spf13/viper v1.19.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/tetratelabs/wazero v1.9.0 // indirect
	github.com/therootcompany/xz v1.0.1 // indirect
	github.com/ulikunitz/xz v0.5.12 // indirect
	github.com/wasilibs/go-re2 v1.9.0 // indirect
	github.com/wasilibs/wazero-helpers v0.0.0-20240620070341-3dff1577cd52 // indirect
	github.com/zricethezav/gitleaks/v8 v8.30.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/collector v0.159.0 // indirect
	go.opentelemetry.io/collector/client v1.65.0 // indirect
	go.opentelemetry.io/collector/component v1.65.0 // indirect
	go.opentelemetry.io/collector/component/componentstatus v0.159.0 // indirect
	go.opentelemetry.io/collector/config/configauth v1.65.0 // indirect
	go.opentelemetry.io/collector/config/configcompression v1.65.0 // indirect
	go.opentelemetry.io/collector/config/configgrpc v1.65.0 // indirect
	go.opentelemetry.io/collector/config/confighttp v0.159.0 // indirect
	go.opentelemetry.io/collector/config/configmiddleware v1.65.0 // indirect
	go.opentelemetry.io/collector/config/confignet v1.65.0 // indirect
	go.opentelemetry.io/collector/config/configopaque v1.65.0 // indirect
	go.opentelemetry.io/collector/config/configoptional v1.65.0 // indirect
	go.opentelemetry.io/collector/config/configtls v1.65.0 // indirect
	go.opentelemetry.io/collector/confmap v1.65.0 // indirect
	go.opentelemetry.io/collector/consumer v1.65.0 // indirect
	go.opentelemetry.io/collector/consumer/consumererror v0.159.0 // indirect
	go.opentelemetry.io/collector/consumer/xconsumer v0.159.0 // indirect
	go.opentelemetry.io/collector/extension/extensionauth v1.65.0 // indirect
	go.opentelemetry.io/collector/extension/extensionmiddleware v0.159.0 // indirect
	go.opentelemetry.io/collector/featuregate v1.65.0 // indirect
	go.opentelemetry.io/collector/internal/componentalias v0.159.0 // indirect
	go.opentelemetry.io/collector/internal/sharedcomponent v0.159.0 // indirect
	go.opentelemetry.io/collector/internal/telemetry v0.159.0 // indirect
	go.opentelemetry.io/collector/pdata v1.65.0 // indirect
	go.opentelemetry.io/collector/pdata/pprofile v0.159.0 // indirect
	go.opentelemetry.io/collector/pipeline v1.65.0 // indirect
	go.opentelemetry.io/collector/pipeline/xpipeline v0.159.0 // indirect
	go.opentelemetry.io/collector/receiver v1.65.0 // indirect
	go.opentelemetry.io/collector/receiver/otlpreceiver v0.159.0 // indirect
	go.opentelemetry.io/collector/receiver/receiverhelper v0.159.0 // indirect
	go.opentelemetry.io/collector/receiver/xreceiver v0.159.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.70.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.70.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	go4.org v0.0.0-20230225012048-214862532bf5 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/exp v0.0.0-20250218142911-aa4b98e5adaa // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
	google.golang.org/grpc v1.83.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/ini.v1 v1.67.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Sibling modules in this multi-module repo; no published version yet, so
// consume them from source. The claude-code adapter transitively pulls in
// client, common/git, and the dev-event conformance module.
replace github.com/openbox-ai/openbox-shift-left/provider => ../provider

replace github.com/openbox-ai/openbox-shift-left/adapters/claude-code => ../adapters/claude-code

replace github.com/openbox-ai/openbox-shift-left/adapters/codex => ../adapters/codex

replace github.com/openbox-ai/openbox-shift-left/adapters/common/git => ../adapters/common/git

replace github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig => ../adapters/common/devconfig

replace github.com/openbox-ai/openbox-shift-left/client => ../client

replace github.com/openbox-ai/openbox-shift-left/telemetry => ../telemetry

replace github.com/openbox-ai/openbox-shift-left/gateway => ../gateway

replace github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance => ../contracts/dev-event/conformance

replace github.com/openbox-ai/openbox-shift-left/decision => ../decision

replace github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow => ../adapters/common/hookflow
