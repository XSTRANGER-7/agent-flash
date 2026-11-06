.PHONY: build test vet run tunnel smoke keygen docker

build:
	go build -trimpath -o bin/arena ./cmd/arena

test:
	go test -race ./...

vet:
	gofmt -l . && go vet ./...

keygen: build
	./bin/arena keygen

smoke: build
	./bin/arena smoke -wallet trendhunter -test

# One command: build, doctor, server (paper money). See run.sh --help.
run:
	./run.sh paper

tunnel:
	./run.sh paper --tunnel

docker:
	docker build -t agent-arena:latest .
