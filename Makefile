.PHONY: build test e2e lint image

VERSION ?= dev

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/cpd ./cmd/cpd

test:
	go vet ./...
	go test -race ./...

# Real binary, real aria2c, stub ComfyUI. Needs aria2c, curl, jq.
e2e:
	test/e2e.sh

lint:
	golangci-lint run

image:
	docker build --build-arg VERSION=$(VERSION) -t comfy-portal-cloud-server:dev .
