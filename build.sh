# Build cross-platform binaries

set -e

GOOS_LIST=("linux" "darwin")
GOARCH_LIST=("amd64" "arm64")
OUTPUT_DIR="build"
mkdir -p $OUTPUT_DIR

for GOOS in "${GOOS_LIST[@]}"; do
  for GOARCH in "${GOARCH_LIST[@]}"; do
    OUTPUT_NAME="$OUTPUT_DIR/unmap-$GOOS-$GOARCH"
    echo "Building for $GOOS/$GOARCH..."
    GOOS=$GOOS GOARCH=$GOARCH go build -o $OUTPUT_NAME main.go

    echo "Built $OUTPUT_NAME"
  done
done