# podswitch, run `just` to list the recipes

default:
    @just --list

# gofmt, vet and the tests
check:
    test -z "$(gofmt -l .)"
    go vet ./...
    go test ./...

# Run against a cluster: just run https://host:6443 path/to/token path/to/ca.crt
run api token ca:
    PODSWITCH_API={{api}} PODSWITCH_TOKEN={{token}} PODSWITCH_CA={{ca}} PODSWITCH_LISTEN=127.0.0.1:8080 go run .

# Static binary for this machine
build:
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o podswitch .

# Container image
image:
    docker build -t podswitch .
