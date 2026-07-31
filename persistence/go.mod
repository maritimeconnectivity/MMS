module github.com/maritimeconnectivity/MMS/persistence

go 1.26.0

require (
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	github.com/mattn/go-sqlite3 v1.14.49
	google.golang.org/protobuf v1.36.11
)

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp
