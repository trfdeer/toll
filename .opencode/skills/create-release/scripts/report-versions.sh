#!/usr/bin/env bash
# Print every version toll currently produces. Read-only: evaluates Nix
# attributes and compiles the Go binary, but does not build the Nix image.
set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"

echo "git          $(git describe --tags --always --dirty 2>/dev/null || git rev-parse --short HEAD)"
echo "plain go     $(go run ./cmd/toll --version)"
echo "nix .#toll   $(nix eval --raw .#toll.version)"
echo "nix .#web    $(nix eval --raw .#web.version)"
echo "nix node_mod $(nix eval --raw .#web-node-modules.version)"
echo "nix .#image  $(nix eval --raw .#image.imageName):$(nix eval --raw .#image.imageTag)"