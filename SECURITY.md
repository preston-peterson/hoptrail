# Security Policy

Hoptrail is a self-hosted network path tracker maintained as a one-person
hobby project. Security issues are taken seriously, but please calibrate
expectations to a best-effort, no-SLA project.

## Supported Versions

Only the **latest released version** receives security patches. If you
find an issue, please test against the latest release before reporting;
if the issue persists there, the report will be acted on.

## Reporting a Vulnerability

**Please do not file a public GitHub issue for security vulnerabilities.**
Use GitHub's private vulnerability reporting:

1. Go to the **Security** tab of the hoptrail repository.
2. Click **Report a vulnerability**.
3. Submit the form.

A useful report includes:

- The hoptrail version (from the `VERSION` file in the install directory).
- A clear description of the issue.
- Steps to reproduce, if possible.
- The potential impact (data exposure, command execution, denial of
  service, privilege escalation, etc.).
- A proof-of-concept if you're comfortable sharing one.

### What to expect

- **Acknowledgment:** best effort within a week. There is no SLA.
- **Triage:** the report will be assessed for severity, reproducibility,
  and scope.
- **Fix:** if the issue is confirmed and in scope, a patch lands in the
  next release. Critical issues may get an out-of-band patch release.
- **Disclosure:** coordinated disclosure is preferred. The fix will
  appear in the changelog without exploit details until users have had
  a reasonable window to update.

## Threat Model

Hoptrail assumes the deployment posture of a **trusted local network**.
The web UI has no authentication by default because LAN-only deployments
don't typically need it, and requiring authentication for the LAN case
would add friction without meaningful protection. This is a deliberate
design choice, not an oversight.

If you expose hoptrail to the public internet or an untrusted network,
**that's a configuration choice you make as the operator**. In that
case, you're responsible for providing authentication and access control
upstream of hoptrail — for example, a reverse proxy with HTTP basic
auth, a VPN, or Tailscale. Issues that exist only because hoptrail is
exposed to the internet are out of scope for this project.

### ICMP privileges

Hoptrail probes the network using ICMP. For traceroute-style probing we
need to receive ICMP Time Exceeded responses from intermediate routers,
which on Linux requires a raw ICMP socket (`SOCK_RAW + IPPROTO_ICMP`).
That socket type requires the `CAP_NET_RAW` capability.

The project's stance is that running hoptrail as root is **not
acceptable** and the daemon will not document that as a supported mode.
The supported privilege model:

- **Capability-based.** The hoptrail binary carries `cap_net_raw+ep`,
  granted at install time:

      sudo setcap cap_net_raw+ep /usr/local/bin/hoptrail

  After this, the daemon runs as an unprivileged user. The capability
  authorizes raw network sockets — nothing else. No filesystem
  privileges, no signal privileges, no process-tree privileges. The
  install script and systemd unit are responsible for setting this up
  during install and verifying it on every start.

The "unprivileged ICMP socket" path (`SOCK_DGRAM + IPPROTO_ICMP`, also
known as Linux's "ping socket") is **not** suitable for hoptrail and is
not supported. It only delivers ICMP Echo Reply to the normal `recv()`
path; Time Exceeded responses go to the socket's error queue and are
not seen on the standard receive path. It was designed for end-to-end
ping, not traceroute.

Any documentation, default config, or installer step that has hoptrail
running as root is a bug — please report it.

## Scope

**In scope** (please report):

- Code-execution, command-injection, or arbitrary-file-write
  vulnerabilities in the hoptrail codebase.
- SQL injection or unsafe query construction.
- Path traversal in endpoints that handle filenames or paths.
- Information disclosure beyond what the application is documented to
  expose.
- Logic flaws that allow unintended configuration changes or data
  manipulation, even on a LAN-only deployment.
- Privilege-escalation paths via the install script, systemd unit, or
  ICMP capability handling.
- Issues that would allow an unprivileged local user to influence
  hoptrail's probe behavior (e.g. forcing it to probe arbitrary targets,
  amplifying outbound ICMP volume).

**Out of scope** (please report upstream or to the appropriate party):

- Issues in third-party dependencies (`pro-bing`, the SQLite driver,
  the HTTP framework) — please report to the upstream project.
- "The web UI has no authentication" — by design, see the threat model
  above.
- Issues only exploitable when hoptrail is intentionally exposed to an
  untrusted network.
- Denial-of-service via traffic flooding — hoptrail has no rate limiting,
  and LAN deployment is the assumed posture.
- Theoretical issues without a clear exploitation path.
- Best-practice deviations that don't represent an exploitable
  vulnerability.

## Hardening Suggestions

A few practices worth following on any hoptrail deployment:

- Run hoptrail on a LAN-only or VPN-only network. Don't expose its HTTP
  port to the public internet.
- Confirm `setcap cap_net_raw+ep` is in place on the hoptrail binary so
  the daemon does not need to run as root. Verify with:

      getcap "$(which hoptrail)"
- Keep the host system patched.
- Review the live `config.yaml` after updates. Newly-added keys land
  with sensible defaults; values you've changed are preserved.

Thanks for helping keep hoptrail secure.
