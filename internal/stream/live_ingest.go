package stream

import (
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
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
var liveSourcePassword = "Test123" // load from config / env

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
	if user != "source" {
		return errors.New("invalid user")
	}
	if pass != liveSourcePassword {
		return errors.New("invalid password")
	}
	return nil
}

func (s *Studio) HandleLiveIngest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "Icecast 2.4.0")
	// w.WriteHeader(http.StatusOK)
	// Accept PUT or SOURCE (Icecast) or POST (some tools)
	if r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != "SOURCE" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Printf("[live %s] incoming method=%s remote=%s", s.ID, r.Method, r.RemoteAddr)

	// Auth
	if err := checkIcecastAuth(r); err != nil {
		log.Printf("[live %s] auth failed: %v", s.ID, err)
		w.Header().Set("WWW-Authenticate", `Basic realm="source"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Reject if one already active
	if s.liveActive.Load() {
		http.Error(w, "live source already active", http.StatusConflict)
		return
	}

	// Capture metadata
	meta := extractLiveMeta(r)
	s.setLiveMeta(meta)

	// Mark active
	s.liveMu.Lock()
	s.liveIngest = r.Body
	s.liveActive.Store(true)
	s.liveMu.Unlock()

	log.Printf("[live %s] connected: name=%q bitrate=%s", s.ID, meta.Name, meta.Bitrate)

	// Respond OK so BUTT shows “connected”
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	buf := make([]byte, 8192)
	graceStart := time.Now()
	earlyEOFs := 0
	for {
		n, err := s.liveIngest.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.push(chunk)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && n == 0 && time.Since(graceStart) < 1500*time.Millisecond && earlyEOFs < 2 {
				earlyEOFs++
				log.Printf("[live %s] early EOF (attempt %d) - retrying brief grace", s.ID, earlyEOFs)
				time.Sleep(150 * time.Millisecond)
				continue
			}
			log.Printf("[live %s] READ end n=%d err=%v", s.ID, n, err)
			break
		}
	}

	s.liveMu.Lock()
	if s.liveIngest != nil {
		_ = s.liveIngest.Close()
		s.liveIngest = nil
	}
	s.liveActive.Store(false)
	s.clearLiveMeta()
	s.liveMu.Unlock()

	log.Printf("[live %s] ended", s.ID)
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
