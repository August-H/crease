.PHONY: build test check fmt release benchmark stress

build:
	go build -trimpath -o bin/crease ./cmd/crease

test:
	go test -race -timeout 120s ./...

check:
	go vet ./...
	test -z "$$(gofmt -l cmd internal scripts/benchserver)"
	go test -race -timeout 120s ./...

fmt:
	gofmt -w cmd internal scripts/benchserver

release:
	python3 scripts/release.py

benchmark: build
	python3 scripts/benchmark.py --binary bin/crease

stress: build
	python3 scripts/stress.py --binary bin/crease
