.PHONY: all edgerouter router genkey

all: edgerouter router genkey

edgerouter:
	cd edgerouter && go build -o ../bin/edgerouter edgerouter.go

router:
	cd router && go build -o ../bin/router router.go

genkey:
	cd genkey && go build -o ../bin/genkey genkey.go

clean:
	rm -rf bin
