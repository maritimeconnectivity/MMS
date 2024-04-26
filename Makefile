.PHONY: all beacon consumer edgerouter router mmtp auth errors revocation read write clean

all: beacon consumer edgerouter router mmtp auth errors revocation read write

beacon:
	cd beacon && go build -o ../bin/beacon beacon.go

consumer:
	cd consumer && go build -o ../bin/consumer consumer.go

edgerouter:
	cd edgerouter && go build -o ../bin/edgerouter edgerouter.go

router:
	cd router && go build -o ../bin/router router.go

mmtp:
	cd mmtp && go build -o ../bin/mmtp mmtp.pb.go

auth:
	cd utils/auth && go build -o ../../bin/auth auth.go

errors:
	cd utils/errors && go build -o ../../bin/errors errors.go

revocation:
	cd utils/revocation && go build -o ../bin/revocation revocation.go

read:
	cd utils/rw && go build -o ../bin/read read.go

write:
	cd utils/rw && go build -o ../bin/write write.go

clean:
	rm -f ./bin/*
