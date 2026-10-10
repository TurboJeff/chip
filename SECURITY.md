# Security

## Reporting a vulnerability

Please don't open a public issue for a security problem. Report it privately through GitHub: go to the **Security** tab of this repository and choose **Report a vulnerability**. Only the maintainer can see the report.

Include what you found, how to reproduce it, and which version or commit you tested.

## Scope

- The chip agent in this repository: what it forwards, what it reports to the hub, and how it handles its token and upstream keys.
- The protocol between the chip and the hub, in `tunnel/` and `proto/`.
- Problems you find with the hub at hub.chipin.dev can be reported the same way.

Fixes go into the latest version on `main`. There are no older release branches.
