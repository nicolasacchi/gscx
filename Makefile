VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test lint tidy clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/gscx ./cmd/gscx

# Always install through make: plain `go install` stamps version "dev".
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/gscx

test:
	go test -race -cover ./...

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/
