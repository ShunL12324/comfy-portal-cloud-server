.PHONY: build test e2e lint image vast-smoke

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

# Rents a cheap GPU on vast.ai and tests the published image on it. Costs cents.
vast-smoke:
	@test -n "$(IMAGE)" || { echo "usage: make vast-smoke IMAGE=ghcr.io/shunl12324/comfy-portal-cloud-server:sha-..."; exit 1; }
	IMAGE=$(IMAGE) test/vast-smoke.sh
