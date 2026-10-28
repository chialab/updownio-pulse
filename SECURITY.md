# Security Policy

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

Please **do not** open a public issue for security problems.
Instead, use GitHub's [private vulnerability reporting](https://github.com/chialab/updownio-pulse/security/advisories/new), or write to [security-notices@chialab.it](mailto:security-notices@chialab.it).

Include as much detail as you can: affected version, steps to reproduce, and impact.
We'll acknowledge your report as soon as possible and keep you posted on the fix.

## Keep your pulse URL secret

Your pulse URL works like a password: anyone who knows it can send pulses and hide real failures.
Avoid committing it to public repositories, and redact it from logs and issues.
If it leaks, regenerate it from the updown.io dashboard.
