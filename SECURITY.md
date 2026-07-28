# Security reporting

TimeTravelContainer is currently intended for trusted development and test
workloads, but TTC-specific changes must not introduce host sandbox escapes or
unintended privilege-escalation paths.

## TTC-specific vulnerabilities

Report vulnerabilities introduced by TimeTravelContainer privately through
[GitHub Security Advisories](https://github.com/gzz2000/TimeTravelOS/security/advisories/new).
Please include the affected commit or version, host configuration, container
image, reproduction steps, and the expected security impact. Do not open a
public issue before a fix or coordinated disclosure is ready.

## Vulnerabilities inherited from gVisor

If the issue also reproduces on an unmodified current gVisor build, follow
[gVisor's security policy](https://github.com/google/gvisor/security/policy)
and report it to the gVisor security team. A report may be sent to both
projects when it is unclear whether TTC-specific code is involved.

## Scope

The following documented TTC choices are not vulnerabilities by themselves:

- branches in one family are not security boundaries from each other;
- `--unsafe-no-host-isolation` deliberately removes host defense-in-depth and
  is restricted to trusted workloads; and
- external resources that TTC explicitly rejects or disconnects during live
  fork are compatibility limitations.

An escape from the Sentry to the host, an unintended cross-family access, or a
privilege escalation not implied by those choices remains in scope.
