package main

import "fmt"

// Version/GitCommit/BuildDate are injected via -ldflags at release build
// time (see .goreleaser.yaml), matching the pattern spawn/truffle already
// use — "dev"/"unknown" are the correct values for a local `go build`.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

func printVersion() {
	fmt.Printf("calque %s\n", Version)
	fmt.Printf("  git commit: %s\n", GitCommit)
	fmt.Printf("  build date: %s\n", BuildDate)
}
