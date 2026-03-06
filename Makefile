.PHONY: build build-arm64 run test clean

build:
	go build -o bin/debrid-vault ./main.go

build-arm64:
	GOOS=linux GOARCH=arm64 go build -o bin/debrid-vault-arm64 ./main.go

run:
	go run main.go

test:
	go test ./... -v

clean:
	rm -rf bin/
