//go:build windows && (amd64 || arm64)

package main

// defaultDevice selects the default WASAPI capture endpoint on Windows.
const defaultDevice = "default"
