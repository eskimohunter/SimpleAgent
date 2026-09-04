VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(if $(VERSION),$(VERSION),dev)

BIN := bin/simpleagent
BIN_WIN := bin/simpleagent.exe

.PHONY: all build build-windows dist test vet fmt clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/simpleagent

build-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_WIN) ./cmd/simpleagent

dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/simpleagent-linux-amd64 ./cmd/simpleagent
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/simpleagent-windows-amd64.exe ./cmd/simpleagent
	cd dist && sha256sum simpleagent-linux-amd64 simpleagent-windows-amd64.exe > SHA256SUMS

test:
	CGO_ENABLED=0 go test ./...

vet:
	CGO_ENABLED=0 go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin dist
