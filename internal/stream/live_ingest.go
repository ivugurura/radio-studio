package stream

import (
	"encoding/base64"
	"errors"
	"log"
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

// Configure per studio if you want different passwords later
var liveSourcePassword = "Test123" // TODO: load from config / env

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

	// Capture metadata
	meta := extractLiveMeta(r)
	s.setLiveMeta(meta)

	// Go's request body works for SOURCE, PUT, and POST, including HTTP/1.0
	// source streams without a Content-Length. Do not hijack the connection:
	// a reverse proxy terminates that connection and Go may already have audio
	// bytes buffered in r.Body.
	reader := r.Body

	// Some clients send Expect: 100-continue before sending body on PUT/POST.
	if r.Method != "SOURCE" && strings.EqualFold(r.Header.Get("Expect"), "100-continue") {
		w.WriteHeader(http.StatusContinue)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		log.Printf("[live %s] sent 100-continue for %s", s.ID, r.Method)
	}

	// SOURCE streams need the response acknowledged while the request body is
	// still being uploaded. Without full duplex, net/http may stop accepting
	// that body after this response is flushed.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		s.liveMu.Lock()
		s.liveActive.Store(false)
		s.clearLiveMeta()
		s.liveMu.Unlock()
		log.Printf("[live %s] could not enable full duplex: %v", s.ID, err)
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	s.liveMu.Lock()
	s.liveIngest = reader
	s.liveMu.Unlock()

	log.Printf("[live %s] connected: method=%s name=%q bitrate=%s", s.ID, r.Method, meta.Name, meta.Bitrate)

	buf := make([]byte, audioChunkSize)
	bytesReceived := 0
	receivedAudio := false
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			// Backpressure preserves the MP3 byte stream. Dropping an arbitrary
			// chunk corrupts the stream until a decoder finds a later frame.
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

	s.liveMu.Lock()
	if s.liveIngest == reader {
		_ = reader.Close()
		s.liveIngest = nil
		s.liveActive.Store(false)
		s.clearLiveMeta()
	}
	s.liveMu.Unlock()

	log.Printf("[live %s] ended", s.ID)
	// Log AutoDJ resume after live suppression ends (if AutoDJ configured)
	if s.autoDJ != nil {
		log.Printf("[live %s] AutoDJ resumed", s.ID)
	}
}

// Live metadata helpers
func extractLiveMeta(r *http.Request) LiveMeta {
	lm := LiveMeta{
		Name:        r.Header.Get("Ice-Name"),
		Genre:       r.Header.Get("Ice-Genre"),
		Description: r.Header.Get("Ice-Description"),
		URL:         r.Header.Get("Ice-URL"),
		Bitrate:     r.Header.Get("Ice-Bitrate"),
		Public:      r.Header.Get("Ice-Public"),
		RawHeaders:  map[string]string{},
		UpdatedAt:   time.Now().UTC(),
	}
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "ice-") {
			lm.RawHeaders[k] = strings.Join(v, ", ")
		}
	}
	return lm
}
