//go:build !(freebsd || linux)

package main

// cloud.guest is only meaningful inside a VM guest (FreeBSD/Linux images).
func registerCloudGuestHandler() {}
