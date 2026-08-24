.PHONY: build test lint check install

build:
	go build -o git-sync ./cmd/git-sync

test:
	go test ./...

lint:
	go vet ./...
	gofmt -l .

check: lint test

install: build
	./git-sync install $(BASE_DIR)
