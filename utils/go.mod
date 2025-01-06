module github.com/maritimeconnectivity/MMS/utils

go 1.22.2

require (
	github.com/coder/websocket v1.8.12
	github.com/google/uuid v1.6.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	golang.org/x/crypto v0.31.0
	google.golang.org/protobuf v1.34.2
)

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp
