module github.com/maritimeconnectivity/MMS/edgerouter

go 1.23.0

toolchain go1.23.1

require (
	github.com/charmbracelet/log v0.4.0
	github.com/coder/websocket v1.8.12
	github.com/google/uuid v1.6.0
	github.com/libp2p/zeroconf/v2 v2.2.0
	github.com/maritimeconnectivity/MMS/consumer v0.0.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	github.com/maritimeconnectivity/MMS/utils v0.0.0
	github.com/prometheus/client_golang v1.20.5

)

require (
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/lipgloss v0.10.0 // indirect
	github.com/go-logfmt/logfmt v0.6.0 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.15 // indirect
	github.com/miekg/dns v1.1.43 // indirect
	github.com/muesli/reflow v0.3.0 // indirect
	github.com/muesli/termenv v0.15.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/exp v0.0.0-20231006140011-7918f672742d // indirect
	golang.org/x/net v0.38.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
	google.golang.org/protobuf v1.36.1 // indirect
)

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp

replace github.com/maritimeconnectivity/MMS/utils => ../utils

replace github.com/maritimeconnectivity/MMS/consumer => ../consumer
