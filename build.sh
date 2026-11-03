#!/usr/bin/env bash
# Compilează nTs-Suite cu toate cele 5 module incluse (un singur executabil).
# Utilizare: ./build.sh            -> dist/ pentru windows-amd64, linux-amd64, linux-arm64
#            ./build.sh linux/amd64 -> doar o platformă
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(pwd)
GH=https://github.com/maxvideostar
REPOS=(nTs-relay:nts-relay ProxyHub:proxyhub rtmp-media-server:rtmp-media-server VideoEncoder:videoencoder Rec-Martori:rec-martori)
if [ $# -gt 0 ]; then TARGETS=("$@"); else TARGETS=(windows/amd64 linux/amd64 linux/arm64); fi
export CGO_ENABLED=0

mkdir -p .src dist
for r in "${REPOS[@]}"; do
  repo=${r%%:*}
  if [ -d ".src/$repo/.git" ]; then git -C ".src/$repo" pull -q --ff-only || true
  else git clone -q --depth 1 "$GH/$repo.git" ".src/$repo"; fi
done

V=$(grep -o 'version = "[^"]*"' main.go | cut -d'"' -f2)
for t in "${TARGETS[@]}"; do
  os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=.exe
  echo "== $os/$arch"
  find bundle -name '*.gz' -delete
  for r in "${REPOS[@]}"; do
    repo=${r%%:*}; bin=${r#*:}
    ( cd ".src/$repo" && GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$ROOT/bundle/$bin$ext" . )
    gzip -9f "bundle/$bin$ext"
    echo "   + $bin$ext"
  done
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "dist/nts-suite-$os-$arch$ext" .
done
find bundle -name '*.gz' -delete
ls -la dist
echo "nTs-Suite $V gata."
