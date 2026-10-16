//go:build pprof

package main

import (
	_ "expvar" // /debug/vars: memory statistics
	"net/http"
	_ "net/http/pprof" // /debug/pprof: CPU, heap, goroutine, mutex and block profiles
	"runtime"
)

// A hub built with -tags pprof serves Go's profiles on PPROF_ADDR, 127.0.0.1:6060 by default, for load tests
// (docs/development.md). Release builds leave this file out.
func init() {
	runtime.SetMutexProfileFraction(10)
	runtime.SetBlockProfileRate(100_000) // one sample per 100 µs spent blocked
	go func() { _ = http.ListenAndServe(env("PPROF_ADDR", "127.0.0.1:6060"), nil) }()
}
