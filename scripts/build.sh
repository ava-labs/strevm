#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

VMID="sr96zN6VeXJ4y5fY5EFziQrPSiy4LJPUMJGQsSLEW4t5bHWPw"
BINARY_PATH="$HOME/.avalanchego/plugins/$VMID"
echo "Building SAE EVM at $BINARY_PATH"
go build -o "$BINARY_PATH" "./plugin/"*.go
echo "Built SAE EVM at $BINARY_PATH"
