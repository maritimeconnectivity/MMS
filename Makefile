BINARIES := edgerouter router genkey
.PHONY: all $(BINARIES) clean test update-tools vet

MODULES := $(BINARIES) consumer mmtp utils
TIDY_MODULES := $(addsuffix .tidy,$(MODULES))
VET_MODULES := $(addsuffix .vet,$(MODULES))
TEST_MODULES := $(addsuffix .test,$(MODULES))
UPDATE_TOOLS_MODULES := $(addsuffix .update-tools,$(MODULES))
.PHONY: $(TIDY_MODULES) $(VET_MODULES) $(VET_CI_MODULES) $(TEST_MODULES) $(UPDATE_TOOLS_MODULES)

all: $(BINARIES)

$(BINARIES):
	cd $@ && go build -o ../bin/$@ $@.go

genkey:
	cd genkey && go build -o ../bin/genkey genkey.go

clean:
	rm -rf bin

tidy: $(TIDY_MODULES)
$(TIDY_MODULES):
	cd $(basename $@) && \
		go mod tidy

update-tools: $(UPDATE_TOOLS_MODULES)
$(UPDATE_TOOLS_MODULES):
	cd $(basename $@) && \
		go get -tool honnef.co/go/tools/cmd/staticcheck@2026.1

vet: $(VET_MODULES)
$(VET_MODULES):
	cd $(basename $@) && \
		go mod tidy && \
		go fmt ./... && \
		go vet ./... && \
		go tool staticcheck ./...

test: $(TEST_MODULES)
$(TEST_MODULES):
	cd $(basename $@) && \
		go test ./...
