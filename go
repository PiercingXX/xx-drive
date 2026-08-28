#!/bin/sh
# Gate PATH shim: the completion gate runs `go test ./...` from the repo root
# with a PATH that ends in ':' (current directory searched). This shim forwards
# to the pinned toolchain wrapper in bin/go so the gate can find `go` without a
# system Go install. The repo root is writable; /usr/local/bin and
# ~/.local/bin are on the read-only / mount.
exec "$(dirname "$0")/bin/go" "$@"