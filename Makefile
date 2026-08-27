.PHONY: test race lint fmt bench cover

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

bench:
	go test -run '^$$' -bench . -benchmem ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out
