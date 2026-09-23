#!/bin/sh

cd "$(dirname "$0")/.." || exit 1

VERSION=$(grep 'const RttyVersion' cmd/rtty/main.go | cut -d'"' -f2 | sed 's/^v//')

GitCommit=$(git log --pretty=format:"%h" -1)
BuildTime=$(date +%FT%T%z)

[ $# -lt 2 ] && {
	echo "Usage: $0 linux amd64"
	exit 1
}

generate() {
	local os="$1"
	local arch="$2"
	local dir="rtty-$VERSION-$os-$arch"
	local bin="rtty"

	rm -rf $dir
	mkdir $dir

	[ "$os" = "windows" ] && {
		bin="rtty.exe"
	}

	GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -ldflags="-s -w -X main.GitCommit=$GitCommit -X main.BuildTime=$BuildTime" -o $dir/$bin ./cmd/rtty

	[ -n "$COMPRESS" ] && {
		tar -jcvf $dir.tar.bz2 $dir
		rm -rf $dir
	}

	exit 0
}

generate $1 $2
