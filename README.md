# Go Streaming Server (Minimal Skeleton)

## Features

- Multi-studio support: `/studio/{studioID}/live` and `/studio/{studioID}/listen`
- Live stream ingest endpoint (for use with encoders like BUTT)
- Listener endpoint (streams live audio to listeners)
- Modular, ready for further dashboard/API integration
- No external dependencies (only Go standard library)

## Usage

1. Build and run the server:

   ```bash
   go run cmd/server/main.go
   ```

2. To start streaming live audio to a studio (from BUTT, etc):

   - Point your encoder to:  
     `http://your-server:8080/studio/studio1/live` (POST/PUT)

3. To listen to a stream:

   - Connect your audio player to:  
     `http://your-server:8080/studio/studio1/listen` (GET)

## Next Steps

- Implement playlist/AutoDJ fallback in `internal/stream/autodj.go`
- Add authentication, admin endpoints, and dashboard integration (when ready)
- Add more robust error handling and logging
