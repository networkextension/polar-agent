//go:build !darwin

package main

// cloud.vm needs Virtualization.framework — macOS only.
func registerCloudVMHandler() {}
