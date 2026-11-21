#!/bin/sh
set -eu
cd "$(dirname "$0")"

profile=off
if [ -f forge.pgo ]; then
  profile=forge.pgo
fi

set --
for source in ./*.go; do
  case "$source" in
    *_test.go | ./epoll_other.go) ;;
    *) set -- "$@" "${source#./}" ;;
  esac
done

GO111MODULE=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
  go build -trimpath -ldflags='-s -w' -pgo="$profile" -o forge-go "$@"
