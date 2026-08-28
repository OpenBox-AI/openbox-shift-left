module github.com/openbox-ai/openbox-shift-left/provider

go 1.27.0

require github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig v0.0.0

require (
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
)

replace github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig => ../adapters/common/devconfig
