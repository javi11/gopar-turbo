//go:build linux

package main

func maxRSSToBytes(v int64) int64 { return v * 1024 }
