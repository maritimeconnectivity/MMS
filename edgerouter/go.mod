module github.com/maritimeconnectivity/MMS/edgerouter

go 1.21

require (
	github.com/google/uuid v1.4.0
	github.com/libp2p/zeroconf/v2 v2.2.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	golang.org/x/crypto v0.16.0
	google.golang.org/protobuf v1.31.0
	nhooyr.io/websocket v1.8.10
)

require (
	github.com/miekg/dns v1.1.43 // indirect
	golang.org/x/net v0.17.0 // indirect
	golang.org/x/sys v0.15.0 // indirect
)

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp
