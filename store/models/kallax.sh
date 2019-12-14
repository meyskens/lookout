#!/usr/bin/env bash

set -e

export GO111MODULE=off
export GOPATH=/tmp/kallax

FILE=kallax.go

mkdir -p $GOPATH

if [ -f "$FILE" ]; then
    mv $FILE $FILE.old
fi

for PKG in $(go list -f '{{ join .Imports "\n" }}' | grep '\.'); do
    (cd "$GOPATH"; go get "$PKG")
done

if [ -f "$FILE.old" ]; then
    mv $FILE.old $FILE
fi

kallax $@