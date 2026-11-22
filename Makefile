BINARY := capy2api
VERSION ?= dev

.PHONY: build run fmt vet clean

build:
	go build -ldflags "-X github.com/haohaomin/capy2api/internal/server.Version=$(VERSION)" -o $(BINARY) ./cmd/server

run:
	go run ./cmd/server

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
