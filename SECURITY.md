# Security policy

## Supported versions

Only the latest release gets fixes. Update with `sudo camdvd update`.

## Reporting a vulnerability

Please **don't open a public issue.** Report it privately through
[GitHub's advisory form](https://github.com/dnaidoo621/camdvd-rescue/security/advisories/new)
(Security › Report a vulnerability).

Include what an attacker can do, the steps to reproduce, and the version
(`camdvd version`). You'll get an acknowledgement within a week. Fixes are
released as soon as they're ready, with credit in the advisory unless you'd
rather not be named.

## Scope and threat model

CamDVD Rescue is a LAN tool. It assumes the network it listens on is
trusted; by default there is **no login**, and anyone who can reach the port
can rip, rename and download files. Set `CAMDVD_PASSWORD` if others share
your network, and never expose the port to the internet directly — use
Tailscale or a reverse proxy with TLS.

In scope:

- Reading or writing files outside the configured library or state folder
- Running commands, or arguments to the external tools, from crafted input
  (disc contents, file names, form fields)
- Bypassing the password login, the same-origin check, or session cookies
- Escaping the systemd sandbox or gaining privileges from the `camdvd` user
- The installer or `camdvd update` installing something unverified

Out of scope: denial of service by someone already on the trusted LAN, and
vulnerabilities in FFmpeg, dvd-vr or distro packages themselves (report those
upstream; we'll update the pinned versions).

## Verifying a release

Each release bundle has a `.sha256` file, which `install.sh` and
`camdvd update` check, and a signed build-provenance attestation:

```sh
gh attestation verify camdvd-<version>-linux-x64.tar.gz --repo dnaidoo621/camdvd-rescue
```
