# polar-agent on FreeBSD (rc.d)

```
install -m 0555 polar-agent-launch.sh /usr/local/bin/polar-agent-launch.sh
install -m 0555 polar_agent /usr/local/etc/rc.d/polar_agent
sysrc polar_agent_enable=YES polar_agent_runas=swift polar_agent_home=/home/swift
service polar_agent start            # log: ~/.polar/agent.log, pid: /var/run/polar_agent.pid
```

- daemon(8) runs as root and drops to the agent user via `su -m` (daemon `-u` fails
  "failed to set user environment" for users without a login class; and rc.subr
  treats `${name}_user` specially — hence `_runas`).
- The wrapper exports HOME from passwd (su -m keeps root's `HOME=/`) and the
  routing/firewall dial-back bases; override with `POLAR_AGENT_ROUTING_BASE` /
  `POLAR_AGENT_FW_BASE` in the wrapper or rc.conf.
- Privilege: sudo if present, else `doas -n` (`permit nopass keepenv <user>` in doas.conf).
  Verified on dell3000b (FreeBSD amd64) 2026-08-23: route_apply committed + rolled back.
