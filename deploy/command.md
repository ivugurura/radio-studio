# Radio Studio VPS Reference

Replace values in angle brackets, such as `<server-host>` and `<studio-id>`,
with the values used by the VPS deployment.

## Service Operations

```sh
# Inspect the service and its most recent failure details.
sudo systemctl status radio-studio --no-pager

# Start, stop, restart, or reload systemd unit definitions after editing them.
sudo systemctl start radio-studio
sudo systemctl stop radio-studio
sudo systemctl restart radio-studio
sudo systemctl daemon-reload

# Enable the service to start after a VPS reboot.
sudo systemctl enable radio-studio

# Follow live application logs.
sudo journalctl -u radio-studio -f

# Show recent logs or only logs from the current boot.
sudo journalctl -u radio-studio -n 200 --no-pager
sudo journalctl -u radio-studio -b --no-pager
```

## Deploy and Verify

```sh
# From the radio-studio checkout on the VPS.
cd <radio-studio-dir>
git status
git pull --ff-only
go test ./...
sudo systemctl restart radio-studio

# Confirm the process is listening on the configured Studio port.
sudo ss -ltnp | grep ':7081'

# Check a Studio health endpoint from the VPS.
curl -i http://127.0.0.1:7081/studios/<studio-id>/health

# Inspect current live/listener state.
curl -s http://127.0.0.1:7081/studios/<studio-id>/status
curl -s http://127.0.0.1:7081/studios/<studio-id>/snapshot
```

## Live Encoder Ingest

BUTT's Icecast `SOURCE` mode uses an HTTP/1.0, connection-delimited upload.
It must reach the Studio server directly or through an Nginx `stream` (TCP)
proxy. Do not route it through an Nginx HTTP `location` proxy.

```text
BUTT server: <server-host>
BUTT port:   7084             # Nginx stream/TCP proxy port, if configured
Mount:       /studios/<studio-id>/live
Protocol:    Icecast / SOURCE
TLS:         disabled for the raw TCP listener
```

If no TCP proxy is configured, use the Studio port directly instead:

```text
BUTT server: <server-host>
BUTT port:   7084
Mount:       /studios/<studio-id>/live
```

Expected source connection logs:

```text
[live <studio-id>] connected: method=SOURCE ...
[live <studio-id>] first audio received (bytes=...)
```

Useful source diagnostics:

```sh
# Show only live-ingest lifecycle messages.
sudo journalctl -u radio-studio -f | grep --line-buffered '\[live '

# Confirm the public raw source port is reachable from a different machine.
nc -vz <server-host> 7084

# Confirm firewall rules expose the encoder port but not the internal Studio port.
sudo ufw status numbered
sudo ss -ltnp | grep -E ':(7081|7084)'
```

## Listener Checks

```sh
# Headers only. A successful listener endpoint returns audio/mpeg.
curl -I http://127.0.0.1:7081/studios/<studio-id>/listen

# Save a short sample while a source or AutoDJ is active; stop with Ctrl+C.
curl -N http://127.0.0.1:7081/studios/<studio-id>/listen -o /tmp/<studio-id>.mp3

# Inspect the resulting audio file, if ffprobe is installed.
ffprobe /tmp/<studio-id>.mp3
```

## Configuration Reference

The systemd unit should provide these environment variables as needed:

```text
LISTEN_ADDR=7081
AUDIO_DIR=<audio-directory>
BACKEND_API=<backend-api-base-url>
BACKEND_API_KEY=<studio-api-key>
DEFAULT_BR_KBPS=128
DEFAULT_SR_HZ=48000
DEFAULT_CH=2
DEFAULT_TRACK_FILE=<fallback-mp3-path>
ALLOWED_ORIGINS=<comma-separated-browser-origins>
```

Live-ingest username/password are no longer env vars — radio-studio fetches
them from the backend (`GET /api/studios/<studio-id>/streaming-config`,
`BACKEND_API_KEY` must match the backend's `STUDIO_TOKEN`) on startup and
every 5 minutes after. Rotate the password from the admin "Streaming Apps"
page.

Review the effective systemd configuration without printing secret values:

```sh
sudo systemctl cat radio-studio
sudo systemctl show radio-studio --property=EnvironmentFiles --property=ExecStart
```

## Nginx Checks

```sh
# Validate and reload after changing HTTP or stream configuration.
sudo nginx -t
sudo systemctl reload nginx

# Verify whether the stream module is installed and loaded.
sudo nginx -V 2>&1 | grep -- --with-stream
ls /etc/nginx/modules-enabled/*stream* 2>/dev/null

# Review Nginx errors and access activity.
sudo tail -n 200 /var/log/nginx/error.log
sudo tail -n 200 /var/log/nginx/access.log
```

## Recovery Checklist

```sh
# 1. Check service health and logs.
sudo systemctl status radio-studio --no-pager
sudo journalctl -u radio-studio -n 200 --no-pager

# 2. Check the local process and endpoint.
sudo ss -ltnp | grep ':7081'
curl -i http://127.0.0.1:7081/studios/<studio-id>/health

# 3. Restart the Studio service.
sudo systemctl restart radio-studio

# 4. If BUTT cannot connect, verify port 7084 (TCP proxy) or 7081 (direct)
#    in both the VPS firewall and the cloud provider security group.
```