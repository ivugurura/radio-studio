# Radio Studio

A lightweight Go streaming server for the [Ivugurura](https://github.com/ivugurura) radio platform. It sits between an audio encoder and listeners, relaying live broadcasts in real time and falling back to a scheduled playlist when no live source is connected.

It is one of three services that make up the platform:

| Service                                                 | Role                                                           |
| ------------------------------------------------------- | -------------------------------------------------------------- |
| **radio-studio** _(this repo)_                          | Streaming server — live ingest and audio delivery to listeners |
| [radio-api](https://github.com/ivugurura/radio-backend) | Application backend — auth, studios, media pipeline, analytics |
| [radio-ui](https://github.com/ivugurura/radio-frontend) | Web dashboard consumed by station staff                        |

## Features

- **Multi-studio streaming** — each studio is served independently, isolating one broadcast from another
- **Live ingest** — accepts a source connection from standard streaming encoders
- **AutoDJ fallback** — seamlessly switches to a rotation playlist when a studio has no live source connected
- **Listener delivery** — serves the active audio stream to any standard audio client
- **Listener analytics** — tracks session and playback activity and forwards it to the backend for reporting
- **Optional GeoIP enrichment** of listener sessions, disabled by default
- **Built for reproducible builds** — dependencies are version-locked

## Tech Stack

- Go

## Requirements

Running the server requires a Go toolchain compatible with the version pinned in `go.mod`, network access to the backend API it reports analytics to, and — if enabling GeoIP — a local GeoIP database.

## Getting Started

At a high level:

1. Provide the server with its configuration (listen address, audio storage location, backend integration details) via environment variables — see `config/config.go` for what's read.
2. Build and run the server binary.
3. Point a streaming encoder at a studio's ingest path to go live; when no encoder is connected, the configured fallback track plays automatically.
4. Point any audio client at a studio's listen path to tune in.

Encoder compatibility varies: some streaming protocols use a connection style that doesn't play well with a typical HTTP reverse proxy sitting in front of the server, so ingest traffic is generally best routed directly to the service.

Load-testing utilities and deployment reference material are available under `cmd/loadtest` and [deploy/](deploy/) respectively.

## License

Licensed under the terms in [LICENSE](LICENSE).

## Maintainer

[Jean d'Amour AKIMANIZANYE](https://github.com/AJAkimana)
