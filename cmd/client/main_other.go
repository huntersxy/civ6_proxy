//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "Civ6RelayClient only supports Windows x64")
	os.Exit(1)
}
