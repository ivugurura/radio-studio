package stream

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ivugurura/radio-studio/config"
)

type LiveMeta struct {
	Name        string
	Genre       string
	Description string
	URL         string
	Bitrate     string
	Public      string
	RawHeaders  map[string]string
	UpdatedAt   time.Time
}

// liveSourceReader consumes the connection-delimited body sent by Icecast
// encoders using the non-standard HTTP/1.0 SOURCE method.
type liveSourceReader struct {
	reader *bufio.Reader
	conn   net.Conn
}

func (r *liveSourceReader) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *liveSourceReader) Close() error {
	return r.conn.Close()
}

// Recommended encoder settings for seamless switching with AutoDJ:
// - Codec: MP3
// - Sample Rate: 44.1kHz
// - Channels: Stereo
// - Bitrate: 128kbps CBR (Constant Bitrate)
// This matches typical AutoDJ pacing and minimizes codec/bitrate mismatches at splice points.

// BasicAuth check for Icecast-like request
func checkIcecastAuth(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return errors.New("missing auth")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Basic") {
		return errors.New("invalid auth scheme")
	}
	decoded, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("bad base64")
	}
	creds := strings.SplitN(string(decoded), ":", 2)
	if len(creds) != 2 {
		return errors.New("invalid credential format")
	}
	user, pass := creds[0], creds[1]
	cfg := config.LoadConfig()
	if user != cfg.User {
		return errors.New("invalid user")
	}
	if pass != cfg.Password {
		return errors.New("invalid password")
	}
	return nil
}

func (s *Studio) clearLiveIngest(reader io.ReadCloser) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	// Only skip clearing if an active (non-nil) liveIngest belongs to a different session.
	// When liveIngest is nil (setup failed before it was assigned), always clear.
	if reader != nil && s.liveIngest != nil && s.liveIngest != reader {
		return
	}
	if reader != nil {
		_ = reader.Close()
	}
	s.liveIngest = nil
	s.liveActive.Store(false)
	s.clearLiveMeta()
}

func (s *Studio) HandleLiveIngest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "Icecast 2.4.0")
	// Accept PUT, POST (ffmpeg etc.) or SOURCE (Icecast encoders like BUTT)
	if r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != "SOURCE" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Printf("[live %s] incoming method=%s remote=%s contentLength=%d", s.ID, r.Method, r.RemoteAddr, r.ContentLength)
	// Debug: dump headers (could gate behind env flag later)
	for k, v := range r.Header {
		log.Printf("[live %s] hdr %s=%q", s.ID, k, strings.Join(v, ", "))
	}

	// Auth
	if err := checkIcecastAuth(r); err != nil {
		log.Printf("[live %s] auth failed: %v", s.ID, err)
		w.Header().Set("WWW-Authenticate", `Basic realm="source"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Reserve the source before acknowledging the encoder so concurrent connects
	// cannot both receive a successful response.
	s.liveMu.Lock()
	if s.liveIngest != nil || s.liveActive.Load() {
		s.liveMu.Unlock()
		http.Error(w, "live source already active", http.StatusConflict)
		return
	}
	s.liveActive.Store(true)
	s.liveMu.Unlock()

	var reader io.ReadCloser
	connected := false
	defer func() {
		s.clearLiveIngest(reader)
		if connected {
			log.Printf("[live %s] ended", s.ID)
			if s.autoDJ != nil {
				log.Printf("[live %s] AutoDJ resumed", s.ID)
			}
		}
	}()

	meta := extractLiveMeta(r)
	s.setLiveMeta(meta)

	if r.Method == "SOURCE" {
		// BUTT sends `SOURCE ... HTTP/1.0` without Content-Length or chunked
		// framing. net/http therefore exposes r.Body as empty. Hijacking retains
		// its buffered bytes and lets us read the connection-delimited audio.
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		conn, readWriter, err := hijacker.Hijack()
		if err != nil {
			log.Printf("[live %s] could not hijack SOURCE connection: %v", s.ID, err)
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		if _, err := readWriter.WriteString("HTTP/1.0 200 OK\r\nServer: Icecast 2.4.0\r\n\r\n"); err != nil {
			_ = conn.Close()
			log.Printf("[live %s] could not acknowledge SOURCE connection: %v", s.ID, err)
			return
		}
		if err := readWriter.Flush(); err != nil {
			_ = conn.Close()
			log.Printf("[live %s] could not flush SOURCE acknowledgement: %v", s.ID, err)
			return
		}
		reader = &liveSourceReader{reader: readWriter.Reader, conn: conn}
	} else {
		reader = r.Body
		if strings.EqualFold(r.Header.Get("Expect"), "100-continue") {
			w.WriteHeader(http.StatusContinue)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			log.Printf("[live %s] sent 100-continue for %s", s.ID, r.Method)
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			log.Printf("[live %s] could not enable full duplex: %v", s.ID, err)
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	s.liveMu.Lock()
	s.liveIngest = reader
	s.liveMu.Unlock()
	connected = true

	log.Printf("[live %s] connected: method=%s name=%q bitrate=%s", s.ID, r.Method, meta.Name, meta.Bitrate)

	buf := make([]byte, audioChunkSize)
	bytesReceived := 0
	receivedAudio := false
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case s.liveFeed <- chunk:
			case <-s.stop:
				return
			}
			bytesReceived += n
			if !receivedAudio {
				receivedAudio = true
				log.Printf("[live %s] first audio received (bytes=%d)", s.ID, bytesReceived)
			}
		}
		if err != nil {
			if !receivedAudio {
				log.Printf("[live %s] ended before receiving audio: %v", s.ID, err)
			} else {
				log.Printf("[live %s] READ end n=%d err=%v (totalBytes=%d)", s.ID, n, err, bytesReceived)
			}
			break
		}
	}
}

// Live metadata helpers
func extractLiveMeta(r *http.Request) LiveMeta {
	// Resolve bitrate: Ice-Bitrate (BUTT), Icy-Br (ffmpeg), or inside Ice-Audio-Info.
	bitrate := r.Header.Get("Ice-Bitrate")
	if bitrate == "" {
		bitrate = r.Header.Get("Icy-Br")
	}
	if bitrate == "" {
		for _, part := range strings.Split(r.Header.Get("Ice-Audio-Info"), ";") {
			if kv := strings.SplitN(strings.TrimSpace(part), "=", 2); len(kv) == 2 && kv[0] == "bitrate" {
				bitrate = kv[1]
				break
			}
		}
	}

	lm := LiveMeta{
		Name:        r.Header.Get("Ice-Name"),
		Genre:       r.Header.Get("Ice-Genre"),
		Description: r.Header.Get("Ice-Description"),
		URL:         r.Header.Get("Ice-URL"),
		Bitrate:     bitrate,
		Public:      r.Header.Get("Ice-Public"),
		RawHeaders:  map[string]string{},
		UpdatedAt:   time.Now().UTC(),
	}
	for k, v := range r.Header {
		kl := strings.ToLower(k)
		if strings.HasPrefix(kl, "ice-") || strings.HasPrefix(kl, "icy-") {
			lm.RawHeaders[k] = strings.Join(v, ", ")
		}
	}
	return lm
}
