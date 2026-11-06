# Concord Voice Feedback

This is where you report bugs and ask for features in Concord Voice. It holds no source code.

## Before you file

1. Check the [Known Issues list](https://github.com/Concord-Voice/Concord-Voice/issues/39). If your problem is there, add a 👍 to it instead of opening a new issue.
2. Search the [open issues](https://github.com/Concord-Voice/Concord-Voice/issues) for the same report.
3. Never post a security vulnerability here. Email [security@concordvoice.com](mailto:security@concordvoice.com) instead. See the [security policy](https://github.com/Concord-Voice/.github/blob/main/SECURITY.md).

You can also send feedback from inside the app, with Send Feedback in your user panel. It lands here too.

## File an issue

- [Report a bug](https://github.com/Concord-Voice/Concord-Voice/issues/new?template=bug_report.yml)
- [Request a feature](https://github.com/Concord-Voice/Concord-Voice/issues/new?template=feature_request.yml)

The team triages new issues and links each one to the work that fixes it.

## Where the code lives

| Repository | What it holds |
|---|---|
| [Desktop](https://github.com/Concord-Voice/Desktop) | The desktop app for macOS, Windows and Linux, and its releases |
| [Mobile](https://github.com/Concord-Voice/Mobile) | The iOS and Android apps |
| [Self-Hosted](https://github.com/Concord-Voice/Self-Hosted) | The server stack you can run yourself, and its releases |

Download the desktop app from [concordvoice.com/download](https://www.concordvoice.com/download).

## What else is kept here

- Old `desktop-v*` releases. Desktop apps up to v0.2.48 update from this repository, so those releases stay. New releases are published from [Desktop](https://github.com/Concord-Voice/Desktop/releases).
- [CHANGELOG.md](CHANGELOG.md), the release history those old releases link to. Each repository now keeps its own changelog.
- The [Terms of Service](docs/legal/terms-of-service.md), the [Privacy Policy](docs/privacy-policy.md) and the [commercial license](docs/legal/commercial-license.md), which older apps link to.
- The [MinIO source-build materials](infrastructure/docker/minio/SOURCE-BUILD.md) and their [runbook](docs/runbooks/refresh-minio-image.md), which published images point to.

## License

Concord Voice is licensed under the [Concord Voice Source License 1.0](LICENSE). Third-party notices are in [NOTICE.md](NOTICE.md).
