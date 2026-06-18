#!/usr/bin/env node

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');
const fs = require('fs');

const platform = os.platform();
const arch = os.arch();

let binaryName = '';

if (platform === 'darwin') {
  if (arch === 'arm64') {
    binaryName = 'stratum-mcp-darwin-arm64';
  } else {
    binaryName = 'stratum-mcp-darwin-amd64';
  }
} else if (platform === 'linux') {
  if (arch === 'arm64') {
    binaryName = 'stratum-mcp-linux-arm64';
  } else {
    binaryName = 'stratum-mcp-linux-amd64';
  }
} else if (platform === 'win32') {
  if (arch === 'x64') {
    binaryName = 'stratum-mcp-windows-amd64.exe';
  }
}

if (!binaryName) {
  console.error(`Unsupported platform/architecture: ${platform}/${arch}`);
  process.exit(1);
}

const binaryPath = path.join(__dirname, binaryName);

if (!fs.existsSync(binaryPath)) {
  console.error(`Binary not found at expected path: ${binaryPath}`);
  console.error('Please ensure the package was built or installed correctly.');
  process.exit(1);
}

// Make sure binary is executable on macOS and Linux
if (platform !== 'win32') {
  try {
    fs.chmodSync(binaryPath, 0o755);
  } catch (err) {
    // Ignore error if chmod fails
  }
}

// Spawn the Go binary forwarding all arguments and pipe stdio
const child = spawn(binaryPath, process.argv.slice(2), {
  stdio: 'inherit'
});

child.on('error', (err) => {
  console.error(`Failed to start Stratum MCP Server: ${err.message}`);
  process.exit(1);
});

child.on('close', (code) => {
  process.exit(code || 0);
});
