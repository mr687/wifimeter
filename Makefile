BINARY := wifimeter
INSTALL_DIR := $(HOME)/bin
TARGET := $(INSTALL_DIR)/$(BINARY)
AGENT := gui/$(shell id -u)/com.github.mr687.wifimeter

# CGO off keeps the binary linked against libSystem + libresolv only.
# No GOPROXY override: a fresh clone must be able to fetch deps.
# VERSION falls back to "dev" outside a git checkout.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_BUILD := CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: build test vet fmt check install restart logs uninstall clean

build:
	$(GO_BUILD) -o $(TARGET) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt vet test

install: build
	$(TARGET) install

restart:
	launchctl kickstart -k $(AGENT)

logs:
	tail -50 /tmp/$(BINARY).log

uninstall:
	$(TARGET) uninstall

clean:
	rm -f $(TARGET)
