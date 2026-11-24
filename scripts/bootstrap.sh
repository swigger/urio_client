#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
fetch() {
  dest=$1 repo=$2 revision=$3
  if [ ! -d "$dest" ]; then
    git init "$dest"
    git -C "$dest" remote add origin "https://github.com/urnetwork/$repo"
    git -C "$dest" fetch --depth 1 origin "$revision"
    git -C "$dest" checkout --detach FETCH_HEAD
  fi
  if [ "$(git -C "$dest" rev-parse HEAD)" != "$revision" ]; then
    echo "Dependency $dest differs from pinned revision; preserve your changes and prepare it separately." >&2
    exit 1
  fi
}
fetch connect connect c9948a7176a284f4dc6ffc7798ff9e321943adbb
fetch .deps/glog glog e4773a4579a56a710f4a90b68580d2e82263b114
fetch .deps/gvisor gvisor 551b6e3f1252d776f947863e9a3f5b802833e588
fetch .deps/quic-go quic-go 981433dd3ad638089c757ee40318d7ab8787bde5
go mod download
