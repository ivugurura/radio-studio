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

   - Point your encoder to:  
     `http://your-server:7080/studios/studio1/live` (POST/PUT)

4. To listen to a stream:

   - Connect your audio player to:  
     `http://your-server:7080/studios/studio1/listen` (GET)

## Next Steps

- Implement playlist/AutoDJ fallback in `internal/stream/autodj.go`
- Add authentication, admin endpoints, and dashboard integration (when ready)
- Add more robust error handling and logging
