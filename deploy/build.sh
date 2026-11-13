# Fish 一键交叉编译脚本（Linux/macOS）
set -e
export CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

OUT="$(cd "$(dirname "$0")/.." && pwd)/dist"
mkdir -p "$OUT"

build() {
    GOOS=$1 GOARCH=$2 go build -trimpath -ldflags "-s -w" -o "$OUT/$3" ./cmd/server
    echo "OK  $OUT/$3"
}

build linux amd64 fish-server-linux-amd64
build linux arm64 fish-server-linux-arm64
build darwin amd64 fish-server-darwin-amd64

echo "全部完成，产物见 $OUT"
