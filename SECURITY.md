# Security policy

## Supported versions

Only the latest published release receives security fixes. Older releases and
development builds are not supported release branches.

## Scope

Security-sensitive areas include:

- `pithd` running as root and its Unix socket's peer-credential authorization;
- helper installation, updates and uninstallation;
- capture-route ownership, shutdown and recovery;
- the future update path, including artifact identity, integrity and authorization.

## Report privately

Use [GitHub private vulnerability reporting](https://github.com/Kel077/pith/security/advisories/new).
Include the affected version, impact, expected and actual behavior, and a minimal
reproduction. Coordinate disclosure with the maintainer before publishing details.

Do not post exploit instructions, credentials, private keys, connection logs,
packet captures, or real network and process details in public issues or pull
requests. Never send passwords or private keys, even in a private report.
Sanitize evidence and use documentation addresses such as `192.0.2.0/24` and
`2001:db8::/32`, plus `example.com`, instead of real addresses or host names.
