BINARY := wifimeter
INSTALL_DIR := $(HOME)/bin
TARGET := $(INSTALL_DIR)/$(BINARY)
AGENT := gui/$(shell id -u)/com.user.wifimeter

# CGO off keeps the binary linked against libSystem + libresolv only.
# GOPROXY=off works because modernc.org/sqlite is already in the module cache;
# drop it if the cache is ever cleared.
GO_BUILD := CGO_ENABLED=0 GOPROXY=off go build -ldflags="-s -w"

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
