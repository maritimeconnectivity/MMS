.PHONY: all beacon edgerouter router       

all: beacon edgerouter router

beacon:
	cd beacon && go build -o ../bin/beacon beacon.go

edgerouter:
	cd edgerouter && go build -o ../bin/edgerouter edgerouter.go

router:
	cd router && go build -o ../bin/router router.go

clean:
	rm -rf bin
