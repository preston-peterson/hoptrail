# Contributing to Hoptrail

Hoptrail is a one-person side project. The short version of how to work
with the project from outside:

- **Bug reports:** welcome, best-effort response.
- **Feature suggestions:** welcome in issues, but the bar for "I'll
  build this" is "I want it for myself."
- **Pull requests:** not accepted.
- **Forks:** encouraged. The MIT license invites you to take hoptrail
  somewhere different if it isn't going where you want.

The longer version is below.

## Why pull requests are disabled

Hoptrail is built and maintained by one person, and I keep direct merges
to myself so the codebase stays internally consistent — in formatting,
in style, in which features earn their place and which add complexity
that isn't worth it. Reviewing and merging outside code well takes real
time, and for a project this size I'd rather spend that time building.

That doesn't mean ideas aren't welcome. **They are — please file an
issue.** I want to know what people are running into, what's missing,
what could be better. I'll read every feature suggestion and bug report,
and the things that fit hoptrail's direction will land in future
releases. The "I want it for myself" filter applies (see
[Suggesting features](#suggesting-features) below for what that means),
but a good idea well-explained genuinely moves the project.

The MIT license also keeps the door open the other way: fork freely,
change whatever, ship your own version. If you want hoptrail to do
something it doesn't do today and the answer to "will the maintainer
build this?" turns out to be no, your fork is a real option — not a
polite deflection. Some of the most useful things people do with
open-source projects are fork them.

## Reporting bugs

If something is broken, please open an issue. The more of the following
you can include, the faster I can act on it:

- **What you tried to do** — the user action, not the symptom.
- **What happened instead** — the observed symptom, ideally with a
  screenshot if it's UI-shaped.
- **Hoptrail version** — `cat VERSION` from your install directory, or
  the version shown in the web UI footer.
- **Linux distribution** — output of `lsb_release -a` and `uname -r`.
- **ICMP privilege bit** — run `getcap "$(which hoptrail)"` and paste
  the output. Should show `cap_net_raw=ep` on the binary; if it doesn't,
  the install step that grants the capability didn't take.
- **Target and approximate hop count** — what you're probing and roughly
  how long the path is. Some bugs only show up on long paths or specific
  upstream behaviors.
- **Relevant log output** — the most useful 50–100 lines from
  `sudo journalctl -u hoptrail -n 200 --no-pager`. If the bug is
  reproducible, set log level to `debug` in `config.yaml`, reproduce,
  and paste those lines instead.

If the bug is security-shaped (a privilege escalation, an unintended
filesystem write, anything that breaks the trusted-LAN threat model),
please use GitHub's private vulnerability reporting instead of a public
issue — see [SECURITY.md](SECURITY.md).

## Suggesting features

Feature requests are fine, with a few realities to set expectations:

- **The maintenance bar is high.** Hoptrail aims to do one thing well
  (continuous path probing with per-hop attribution). I'm conservative
  about adding more, especially for use cases I won't personally
  exercise. "I want it for myself" is a real filter, not a polite
  framing.
- **Roadmap is private beyond the milestones in the README.** I don't
  commit to delivery dates. If a request lands in my own backlog, you
  may see it in a future release. If it doesn't, it doesn't.
- **A "no" isn't an indictment of the idea.** It usually means the idea
  is good but not aligned with what I want this codebase to be. That's
  exactly the case where forking makes sense.

## If you're forking

Go ahead. The MIT license has you covered. A few practical pointers
that will accumulate as the project ships:

- **`VERSION`** is the canonical version string. Release tooling drives
  it together with CHANGELOG and any version strings embedded in source.
  Read it before changing versions by hand.
- **The three technical gotchas** — ICMP privilege handling, ECMP route
  bucketing, and loss attribution — are the parts most likely to bite
  if changed casually. Touch them with care.

Good luck, and have fun with it.
