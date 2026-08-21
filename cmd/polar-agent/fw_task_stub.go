//go:build !(darwin || freebsd || linux)

package main

// fw executor 只在 pf(darwin/freebsd)/ nftables(linux)平台注册。
func registerFirewallHandlers() {}
