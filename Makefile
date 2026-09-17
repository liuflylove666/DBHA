GO ?= go
PYTHON ?= python3
MODULE := ./dbm-services/common/dbha-v2
SHARED := ./dbm-services/common/go-pubpkg
SERVICES := admin receiver analysis probe
BINARIES := $(addprefix bin/dbha-,$(SERVICES)) bin/standalone-metadata
export GOWORK := $(CURDIR)/go.work

.PHONY: build test check test-config $(BINARIES)

# Protobuf Go sources are already included; protoc is only needed to regenerate them.
build: $(BINARIES)

$(addprefix bin/dbha-,$(SERVICES)): bin/dbha-%:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o $@ $(MODULE)/cmd/$*

bin/standalone-metadata:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -o $@ $(MODULE)/tools/cmd/standalone-metadata

test: test-config
	$(GO) test $(MODULE)/... $(SHARED)/...

test-config:
	$(PYTHON) -m unittest discover -s deploy/dbha-v2 -p 'test_*.py'
	$(PYTHON) -m unittest discover -s deploy/dbha-v2/ec2 -p 'test_*.py'
	$(PYTHON) -m unittest discover -s dbm-services/common/dbha-v2/scripts -p 'test_*.py'

check:
	$(GO) vet $(MODULE)/... $(SHARED)/...
	@find deploy/dbha-v2 -name '*.sh' -print0 | xargs -0 -n 1 bash -n
