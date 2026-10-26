.PHONY: build test check fmt release benchmark

build:
	go build -trimpath -o bin/crease ./cmd/crease

test:
	go test -race -timeout 120s ./...

check:
	go vet ./...
	test -z "$$(gofmt -l cmd internal)"
	go test -race -timeout 120s ./...

fmt:
	gofmt -w cmd internal

release:
	python3 scripts/release.py

benchmark: build
	python3 scripts/benchmark.py --binary bin/crease
