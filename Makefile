.PHONY: build test test-race vet bench clean server docker-build docker-run

build:
	go build ./...

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

bench:
	go test -bench . -benchmem -run '^$$' ./...

server:
	go run ./cmd/server

docker-build:
	docker build -t strata-server .

docker-run:
	docker compose up --build

clean:
	rm -f server crashwriter
	rm -rf data
