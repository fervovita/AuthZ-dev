.PHONY: all test race integration lint fmt bench cover

all: lint integration

test:
	go test ./...

race:
	go test -race ./...

integration:
	go test -race -tags integration ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

bench:
	go test -run '^$$' -bench . -benchmem ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out
