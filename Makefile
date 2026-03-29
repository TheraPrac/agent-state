.PHONY: build test clean install

build:
	go build -o bin/st ./cmd/as

test:
	go test ./... -cover

clean:
	rm -f bin/st

install: build
	cp bin/st /usr/local/bin/st
