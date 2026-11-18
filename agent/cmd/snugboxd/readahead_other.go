//go:build !linux

package main

// adviseRandomReads does nothing off Linux, where snugboxd does not run.
func adviseRandomReads() error { return nil }

// adviseCold does nothing off Linux, where snugboxd does not run.
func adviseCold() error { return nil }
