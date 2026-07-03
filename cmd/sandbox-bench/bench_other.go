//go:build !linux

package main

import "fmt"

func run(benchConfig) error {
	return fmt.Errorf("sandbox-bench only runs on Linux; run this inside the TF runtime container (scripts/sandbox-bench.sh)")
}
