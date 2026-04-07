.PHONY: build test lint docker helm-lint helm-template clean

BINARY := orchard-gh-bridge
IMAGE  := ghcr.io/breakawaydata/orchard-gh-bridge
TAG    ?= dev

build:
	go build -o $(BINARY) .

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

docker:
	docker build -t $(IMAGE):$(TAG) .

helm-lint:
	helm lint charts/orchard-gh-bridge

helm-template:
	helm template test charts/orchard-gh-bridge

clean:
	rm -f $(BINARY)
