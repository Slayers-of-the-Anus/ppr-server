.PHONY: run build test fmt vet
run:
	go run ./cmd/server
build:
	go build -o bin/phonepilot-server ./cmd/server
test:
	go test ./...
fmt:
	gofmt -w ./cmd ./internal
vet:
	go vet ./...
