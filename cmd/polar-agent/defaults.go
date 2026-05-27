package main

// defaultServer is the canonical Polar control-plane URL all agents
// talk to unless --server is given explicitly. Single source of truth
// — keeps registrations from going to a mix of LAN-IP / WG-IP / public
// hostname, which was the cause of the 2026-05-27 emei "host lost"
// incident (a host registered against http://192.168.11.57:8080 was
// invisible to admins browsing https://zen.4950.store:2443).
//
// Override at build time for forks / private deployments with:
//
//	go build -ldflags "-X main.defaultServer=https://custom.example:443" \
//	    ./cmd/polar-agent
//
// Or override per-invocation with --server=<url> on register / login.
var defaultServer = "https://zen.4950.store:2443"
