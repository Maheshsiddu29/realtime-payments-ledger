package main

import "runtime"

// Small indirections so describeEnv reads cleanly and the runtime import stays
// in one place.

func runtimeVersion() string { return runtime.Version() }

func runtimeOSArch() string { return runtime.GOOS + "/" + runtime.GOARCH }

func runtimeCPUs() int { return runtime.NumCPU() }
