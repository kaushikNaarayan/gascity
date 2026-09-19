//go:build !linux

package main

// Without /proc there is no portable progress signal; init keeps the fixed
// providerOpTimeout deadline.
const providerOpProgressSupported = false

func sampleProcessGroupProgress(int) (uint64, bool) { return 0, false }
