module github.com/maritimeconnectivity/MMS/consumer

go 1.23.0

require (
	github.com/coder/websocket v1.8.14
	github.com/google/uuid v1.6.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	github.com/maritimeconnectivity/MMS/utils v0.0.0
)

require google.golang.org/protobuf v1.36.1 // indirect

replace github.com/maritimeconnectivity/MMS/utils => ../utils

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp
