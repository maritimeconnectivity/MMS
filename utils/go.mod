module github.com/maritimeconnectivity/MMS/utils

go 1.22.2

require (
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0-00010101000000-000000000000
	golang.org/x/crypto v0.22.0
	nhooyr.io/websocket v1.8.11
)

require google.golang.org/protobuf v1.33.0 // indirect

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp
