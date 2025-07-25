#!/bin/bash

set -e

# Build DevKit from tee-mvp branch source
REPO_URL="https://github.com/Layr-Labs/devkit-cli"
BRANCH="tee-mvp"

# Check for required tools
command -v git >/dev/null 2>&1 || { echo "Error: git is required but not installed."; exit 1; }
command -v go >/dev/null 2>&1 || { echo "Error: go is required but not installed."; exit 1; }
command -v make >/dev/null 2>&1 || { echo "Error: make is required but not installed."; exit 1; }

# Prompt for installation directory
if [[ -t 0 ]]; then
    # Interactive terminal available
    echo "Where would you like to install DevKit?"
    echo "1) $HOME/bin (recommended)"
    echo "2) /usr/local/bin (system-wide, requires sudo)"
    echo "3) Custom path"
    read -p "Enter choice (1-3) [1]: " choice
else
    # Non-interactive (piped), use default
    echo "Installing to $HOME/bin (default for non-interactive install)"
    choice=1
fi

case ${choice:-1} in
    1) INSTALL_DIR="$HOME/bin" ;;
    2) INSTALL_DIR="/usr/local/bin" ;;
    3) 
        read -p "Enter custom path: " INSTALL_DIR
        if [[ -z "$INSTALL_DIR" ]]; then
            echo "Error: No path provided"
            exit 1
        fi
        ;;
    *) echo "Invalid choice"; exit 1 ;;
esac

# Create directory if it doesn't exist
if [[ "$INSTALL_DIR" == "/usr/local/bin" ]]; then
    sudo mkdir -p "$INSTALL_DIR"
else
    mkdir -p "$INSTALL_DIR"
fi

# Clone and build from source
TEMP_DIR=$(mktemp -d)
echo "Cloning DevKit from ${REPO_URL} (${BRANCH})..."
git clone --branch "$BRANCH" --depth 1 "$REPO_URL" "$TEMP_DIR"

echo "Building DevKit..."
cd "$TEMP_DIR"
make build

echo "Installing DevKit to $INSTALL_DIR..."
if [[ "$INSTALL_DIR" == "/usr/local/bin" ]]; then
    sudo cp bin/devkit "$INSTALL_DIR/devkit"
    sudo chmod +x "$INSTALL_DIR/devkit"
else
    cp bin/devkit "$INSTALL_DIR/devkit"
    chmod +x "$INSTALL_DIR/devkit"
fi

# Clean up
rm -rf "$TEMP_DIR"

echo "✅ DevKit installed to $INSTALL_DIR/devkit"

# Add to PATH if needed
if [[ "$INSTALL_DIR" == "$HOME/bin" ]] && [[ ":$PATH:" != *":$HOME/bin:"* ]]; then
    echo "💡 Add $HOME/bin to your PATH:"
    echo "   echo 'export PATH=\"\$HOME/bin:\$PATH\"' >> ~/.$(basename $SHELL)rc"
fi

echo "🚀 Verify installation: $INSTALL_DIR/devkit --help"
