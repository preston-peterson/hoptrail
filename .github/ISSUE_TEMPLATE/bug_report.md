---
name: Bug report
about: Report something that's broken or behaving unexpectedly
title: ''
labels: bug
assignees: ''
---

<!--
Hoptrail is a one-person hobby project. Bug reports are welcome and get
a best-effort response. The more of the sections below you can fill in,
the faster the bug becomes actionable.

For security-shaped bugs (privilege escalation, anything that breaks the
trusted-LAN threat model, anything that lets an unprivileged user
influence probe behavior), please use GitHub's private vulnerability
reporting instead of a public issue. See SECURITY.md.
-->

## What you tried to do

<!-- The user action, not the symptom. e.g. "Started hoptrail serve against 8.8.8.8 and opened the web UI." -->

## What happened instead

<!-- The observed symptom. Add a screenshot if it's UI-shaped. -->

## Hoptrail version

<!-- From `cat VERSION` in your install directory, or the version shown in the web UI footer. -->

## Linux distribution and kernel

<!-- Output of `lsb_release -a` and `uname -r`. Helpful because ICMP behavior varies across distros and kernels. -->

## ICMP privilege bit in use

<!--
Hoptrail runs with the `CAP_NET_RAW` capability granted via setcap.
Run:

    getcap "$(which hoptrail)"

It should print something like
`/usr/local/bin/hoptrail cap_net_raw=ep`. If it prints nothing, the
capability is missing and the daemon will fail to open its ICMP
socket. If somehow the daemon is running as root, mention that — it
shouldn't be, and that's itself useful information.
-->

## Target and hop count

<!--
What you're probing (e.g. 8.8.8.8) and roughly how long the path is (the
"hops" count from the UI, or the output of any traceroute tool you have
handy). Some bugs only show up on long paths or specific upstream
behaviors.
-->

## Relevant log output

<!--
The most useful 50–100 lines from:

    sudo journalctl -u hoptrail -n 200 --no-pager

If the bug is reproducible, set `log.level: debug` in config.yaml,
reproduce it, and paste those lines here instead.
-->

```
<paste log output here>
```
