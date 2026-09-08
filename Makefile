.PHONY: build test test-race vet bench clean

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

clean:
	rm -f server crashwriter
	rm -rf data
