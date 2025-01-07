.PHONY: all edgerouter router

all: edgerouter router

edgerouter:
	cd edgerouter && go build -o ../bin/edgerouter edgerouter.go

router:
	cd router && go build -o ../bin/router router.go

clean:
	rm -rf bin
