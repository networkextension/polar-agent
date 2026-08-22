//go:build !(darwin || freebsd || linux || openbsd)

package main

// Route executor / collector only register on OSes with a route(8)/ip(8)
// we know how to drive.
func registerRoutingHandlers() {}

func startRoutingCollector(AgentConfig) {}
