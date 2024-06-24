module github.com/maritimeconnectivity/MMS/consumer

go 1.22.2

require (
	github.com/google/uuid v1.6.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	github.com/maritimeconnectivity/MMS/utils v0.0.0
	nhooyr.io/websocket v1.8.11
)

require google.golang.org/protobuf v1.34.2 // indirect

replace github.com/maritimeconnectivity/MMS/utils => ../utils

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp
