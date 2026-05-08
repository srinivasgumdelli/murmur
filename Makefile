.PHONY: build run clean docker-up docker-down

build:
	go build -o murmur ./cmd/murmur

run: build
	./murmur

clean:
	rm -f murmur

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

lint:
	golangci-lint run ./...

test:
	go test ./...
