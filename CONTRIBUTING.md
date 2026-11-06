# Contributing to Pith

Pith is maintained by one person, so the process is small.

## Issues, not pull requests

Only maintainers open pull requests; the repository is set up that way. To suggest a change, open an issue: a [bug report](https://github.com/Kel077/pith/issues/new?template=bug.yml) or a [feature request](https://github.com/Kel077/pith/issues/new?template=feature.yml). If you have a fix in mind, describe it, or link to a branch in your fork. When a commit uses your work, it credits you with `Co-authored-by`.

## Keep your network private

Issues are public. Don't paste IP addresses, host names, Wi-Fi names, MAC addresses, or the output of `ifconfig`, `netstat`, `route` or `scutil`; write `example.com` or `192.0.2.1` instead, and blur screenshots.

Security problems go to [private vulnerability reporting](https://github.com/Kel077/pith/security/advisories/new); see [SECURITY.md](SECURITY.md).

## Building and testing

With Go 1.27 or later on an Apple silicon Mac:

```sh
go vet ./... && go test ./...
go run ./cmd/pith -mock    # the interface on simulated data, nothing installed
```

Tests use documentation addresses (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32) and `example.com`. Building and testing never change your Mac's network; turning routing on installs the helper, which takes an administrator's password.

## Work done with AI

If AI helped with what you send, say so; you're responsible for having read and checked it.

## License

Contributions are under the [MIT license](LICENSE).
