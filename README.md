# Go Streaming Server (Minimal Skeleton)

## Features

- Multi-studio support: `/studios/{studioID}/live` and `/studios/{studioID}/listen`
- Live stream ingest endpoint (for use with encoders like BUTT)
- Listener endpoint (streams live audio to listeners)
- Modular, ready for further dashboard/API integration
- External modules are tracked in `go.mod` and `go.sum` for reproducible builds
- GeoIP enrichment is optional and disabled unless configured in `internal/geo`

## Usage

1. Build and run the server:

   ```bash
   go run cmd/server/main.go
   ```

2. If you change dependencies, commit both `go.mod` and `go.sum` so CI and deploy builds stay in sync.

3. To start streaming live audio to a studio (from BUTT, etc):

    - For BUTT's Icecast `SOURCE` protocol, point the encoder directly to the
       Studio service port, not an HTTP reverse proxy:
       `http://your-server:7080/studios/reformation-rw/live`
    - `SOURCE` is an HTTP/1.0, connection-delimited upload. A normal HTTP
       reverse proxy can classify it as bodyless because it has neither
       `Content-Length` nor chunked transfer encoding.
    - Encoders that use standard `POST` or `PUT` may be proxied normally.

4. To listen to a stream:

   - Connect your audio player to:  
     `http://your-server:7080/studios/studio1/listen` (GET)

## Next Steps

- Implement playlist/AutoDJ fallback in `internal/stream/autodj.go`
- Add authentication, admin endpoints, and dashboard integration (when ready)
- Add more robust error handling and logging
