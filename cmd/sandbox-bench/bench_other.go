//go:build !linux

package main

import "fmt"

func run(benchConfig) error {
	return fmt.Errorf("the sandbox only runs on Linux; run this inside the TF runtime container (scripts/sandbox-bench.sh)")
}
