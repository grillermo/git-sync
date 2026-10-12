.PHONY: build test lint check install update-mesh

build:
	./build

test:
	go test ./...

lint:
	go vet ./...
	gofmt -l .

check: lint test

install: build
	./bin/git-sync install $(BASE_DIR)

update-mesh: build
	./bin/git-sync update-mesh
