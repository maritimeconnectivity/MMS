module github.com/maritimeconnectivity/MMS/utils

go 1.23.0

require (
	github.com/coder/websocket v1.8.14
	github.com/google/uuid v1.6.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	golang.org/x/crypto v0.37.0
	google.golang.org/protobuf v1.36.1
)

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp
