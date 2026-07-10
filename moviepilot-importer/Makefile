BINARY := media-agent-plugin-moviepilot-importer
GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
OUT    := bin/$(BINARY)-$(GOOS)-$(GOARCH)

.PHONY: build test vet clean

build:
	go build -o $(OUT) .

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f bin/$(BINARY)-*
