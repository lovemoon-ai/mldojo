# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues, discussions or pull requests.**

Report them privately through GitHub Security Advisories:
[github.com/lovemoon-ai/mldojo/security/advisories/new](https://github.com/lovemoon-ai/mldojo/security/advisories/new)
(repository **Security** tab → **Report a vulnerability**).

Please include as much of the following as you can:
- the kind of issue (e.g. authentication bypass, secret disclosure, command injection, SSRF);
- the affected component (`mldojo-api`, `mldojo-agent`, CLI, web UI, deploy scripts, queue plugin protocol) and version or commit;
- steps to reproduce, a proof of concept, and the impact you expect.

We will acknowledge your report as soon as we can, keep you updated while we work on a fix, and credit you in the
advisory unless you prefer otherwise. Please give us a reasonable amount of time to release a fix before any
public disclosure.

## Supported versions

Security fixes are made on the latest release and the `main` branch.

## Scope notes

MLDojo holds credentials (SSH keys, API tokens, queue credentials) and can run commands on registered nodes, so
issues in authentication, authorization, secret handling and agent/SSH communication are especially important to us.
