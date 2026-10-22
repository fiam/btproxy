GO ?= CGO_ENABLED=0 mise exec -- go

# All Go sources except generated code; the generated api_gen.go is
# produced by go generate (see the rules below).
GO_SOURCES := $(shell find cmd internal tinypb tools -name '*.go' ! -name 'api_gen.go' 2>/dev/null)

# Stamp marking that //go:generate directives have been run. Generated
# files (internal/esphome/api_gen.go) are byproducts, not tracked by git;
# building requires the Go toolchain anyway, which is the only consumer of
# the generator.
STAMP := .generated-code

.PHONY: bootstrap build test vet generate generate-code clean install

PREFIX ?= /usr/local

bootstrap:
	mise install

# --- code generation -------------------------------------------------------
#
# The stamp depends on every input of the //go:generate directives in the
# module (the commands themselves are defined next to the code, in
# internal/esphome/messages.go). When any input is newer than the stamp,
# go generate is re-run; generated files are byproducts of that run.

$(STAMP): internal/esphome/api.pidl tinypb/pb.go tinypb/cmd/tinypbc/main.go
	$(GO) generate ./...
	@touch $@

generate: $(STAMP)

# --- build ----------------------------------------------------------------

build: bin/btproxy

bin/btproxy: $(GO_SOURCES) $(STAMP) go.mod go.sum
	$(GO) build -o $@ ./cmd/btproxy

# --- test -----------------------------------------------------------------

test: $(STAMP)
	$(GO) test ./...

vet: $(STAMP)
	$(GO) vet ./...

# Installs the daemon, the systemd unit and (when firewalld is present)
# the firewalld service definition. Installs only — reloading and
# enabling are left to the admin:
#
#	sudo systemctl daemon-reload && sudo systemctl enable --now btproxy
#	sudo firewall-cmd --reload
#	sudo firewall-cmd --permanent --add-service=btproxy && sudo firewall-cmd --reload
install: build
	sudo install -m755 bin/btproxy $(PREFIX)/bin/btproxy
	sudo install -m644 systemd/btproxy.service /etc/systemd/system/btproxy.service
	@if [ -d /etc/firewalld ]; then \
		sudo install -m644 systemd/btproxy-firewalld.xml /etc/firewalld/services/btproxy.xml; \
	fi

clean:
	rm -rf bin $(STAMP) internal/esphome/api_gen.go
