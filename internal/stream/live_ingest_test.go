package stream

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleLiveIngestReceivesAudio(t *testing.T) {
	audio := []byte("mp3-frame-bytes")

	tests := []struct {
		name       string
		requestTop string
		headers    string
		wantStatus string
		body       []byte
	}{
		{
			// The request BUTT tries first: no Content-Length, no chunked framing.
			name:       "unframed PUT with Expect",
			requestTop: "PUT /live HTTP/1.1",
			headers:    "Expect: 100-continue\r\n",
			wantStatus: "HTTP/1.1 100 Continue",
			body:       audio,
		},
		{
			name:       "unframed PUT without Expect",
			requestTop: "PUT /live HTTP/1.1",
			wantStatus: "HTTP/1.0 200 OK",
			body:       audio,
		},
		{
			name:       "SOURCE",
			requestTop: "SOURCE /live HTTP/1.0",
			wantStatus: "HTTP/1.0 200 OK",
			body:       audio,
		},
		{
			name:       "chunked PUT with Expect",
			requestTop: "PUT /live HTTP/1.1",
			headers:    "Transfer-Encoding: chunked\r\nExpect: 100-continue\r\n",
			wantStatus: "HTTP/1.1 100 Continue",
			body:       fmt.Appendf(nil, "%x\r\n%s\r\n", len(audio), audio),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Studio{
				ID:          "test",
				bitrateKbps: 128,
				srHz:        48000,
				ch:          2,
				liveFeed:    make(chan []byte, 8),
				stop:        make(chan struct{}),
			}
			s.SetCredentials("user", "pass")

			srv := httptest.NewServer(http.HandlerFunc(s.HandleLiveIngest))
			defer srv.Close()

			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

			auth := base64.StdEncoding.EncodeToString([]byte("user:pass"))
			fmt.Fprintf(conn, "%s\r\nHost: test\r\nAuthorization: Basic %s\r\n"+
				"ice-audio-info: ice-bitrate=128;ice-channels=2;ice-samplerate=48000\r\n%s\r\n",
				tt.requestTop, auth, tt.headers)

			status, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil && err != io.EOF {
				t.Fatalf("read status: %v", err)
			}
			if got := strings.TrimSpace(status); got != tt.wantStatus {
				t.Fatalf("status = %q, want %q", got, tt.wantStatus)
			}

			if _, err := conn.Write(tt.body); err != nil {
				t.Fatalf("write audio: %v", err)
			}

			select {
			case got := <-s.liveFeed:
				if !bytes.Equal(got, audio) {
					t.Fatalf("liveFeed = %q, want %q", got, audio)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("audio never reached liveFeed")
			}
		})
	}
}
