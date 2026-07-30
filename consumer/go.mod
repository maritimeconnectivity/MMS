module github.com/maritimeconnectivity/MMS/consumer

go 1.26.0

require (
	github.com/coder/websocket v1.8.15
	github.com/google/uuid v1.6.0
	github.com/maritimeconnectivity/MMS/mmtp v0.0.0
	github.com/maritimeconnectivity/MMS/persistence v0.0.0
	github.com/maritimeconnectivity/MMS/utils v0.0.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	honnef.co/go/tools v0.7.0 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.55.0 // indirect
)

replace github.com/maritimeconnectivity/MMS/utils => ../utils

replace github.com/maritimeconnectivity/MMS/mmtp => ../mmtp

replace github.com/maritimeconnectivity/MMS/persistence => ../persistence

tool honnef.co/go/tools/cmd/staticcheck
