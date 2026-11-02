# Security Policy

## Reporting a vulnerability

Open a private security advisory on GitHub (Security, then Advisories) or email the
maintainer. Please do not open public issues for vulnerabilities.

## Controls in this repository

- **Static analysis:** CodeQL on every push to `main` and weekly.
- **Dependencies:** versions are pinned in the lockfile; updates are reviewed and applied by hand.
- **Secrets:** none are committed; configuration comes from the environment.
