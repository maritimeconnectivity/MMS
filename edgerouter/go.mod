module github.com/maritimeconnectivity/MMS/edgerouter

go 1.22.2

require (
	github.com/charmbracelet/log v0.4.0
	github.com/google/uuid v1.6.0
	github.com/libp2p/zeroconf/v2 v2.2.0
	github.com/maritimeconnectivity/MMS/consumer v0.0.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	github.com/maritimeconnectivity/MMS/utils v0.0.0
	nhooyr.io/websocket v1.8.11

)

require (
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/charmbracelet/lipgloss v0.10.0 // indirect
	github.com/go-logfmt/logfmt v0.6.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.15 // indirect
	github.com/miekg/dns v1.1.43 // indirect
	github.com/muesli/reflow v0.3.0 // indirect
	github.com/muesli/termenv v0.15.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	golang.org/x/crypto v0.24.0 // indirect
	golang.org/x/exp v0.0.0-20231006140011-7918f672742d // indirect
	golang.org/x/net v0.23.0 // indirect
	golang.org/x/sys v0.21.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp

replace github.com/maritimeconnectivity/MMS/utils => ../utils

replace github.com/maritimeconnectivity/MMS/consumer => ../consumer
