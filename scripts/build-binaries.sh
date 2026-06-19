#!/bin/bash
set -e

# Detect host platform and architecture
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

# Normalize ARCH
if [ "$ARCH" = "x86_64" ]; then
  ARCH="amd64"
elif [ "$ARCH" = "arm64" ] || [ "$ARCH" = "aarch64" ]; then
  ARCH="arm64"
fi

# Normalize OS for Windows/MinGW/git bash
if [[ "$OS" == *"mingw"* ]] || [[ "$OS" == *"msys"* ]] || [[ "$OS" == *"cygwin"* ]]; then
  OS="windows"
fi

echo "Building Stratum MCP standalone binary for local host: ${OS}/${ARCH}..."

mkdir -p bin

if [ "$OS" = "darwin" ]; then
  if [ "$ARCH" = "arm64" ]; then
    go build -o bin/stratum-mcp-darwin-arm64 main.go
  else
    go build -o bin/stratum-mcp-darwin-amd64 main.go
  fi
elif [ "$OS" = "linux" ]; then
  if [ "$ARCH" = "arm64" ]; then
    go build -o bin/stratum-mcp-linux-arm64 main.go
  else
    go build -o bin/stratum-mcp-linux-amd64 main.go
  fi
elif [ "$OS" = "windows" ]; then
  go build -o bin/stratum-mcp-windows-amd64.exe main.go
else
  echo "Unsupported OS: $OS"
  exit 1
fi

echo "Local compilation complete! Binary saved to bin/."
