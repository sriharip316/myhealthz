BINARY := bin/myhealthz
IMAGE  ?= localhost/myhealthz:dev
VERSION := $(shell git describe --tags --always --dirty)

.PHONY: build test vet image clean demo setup status switchover verify teardown

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) .

test:
	go test ./...

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

vet:
	go vet ./...

lint:
	staticcheck ./...
	golangci-lint run ./...
	govulncheck ./...

image:
	podman build -t $(IMAGE) .

demo: ## full guided demo: setup, status, switchover, status
	./demo/demo.sh demo

setup:
	./demo/demo.sh setup

status:
	./demo/demo.sh status

switchover:
	./demo/demo.sh switchover

verify: ## automated E2E assertions against the demo stack
	./demo/demo.sh verify

teardown:
	./demo/demo.sh teardown

clean:
	rm -rf bin
