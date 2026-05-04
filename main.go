// Package main is a stub. The mkfst framework is consumed as a
// library — there is no runnable program at the repo root. The
// operator/author CLI lives at cmd/mkfst.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mkfst: this is a library; run the CLI via `go run ./cmd/mkfst`")
	os.Exit(2)
}
