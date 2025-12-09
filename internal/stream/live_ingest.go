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
var liveSourcePassword = "Test123" // TODO: load from config / env

// Tunables for handling fragile encoders that briefly close right after connect
var (
	liveEarlyEOFGrace     = 5 * time.Second // total window after connect to tolerate early EOFs
	liveEarlyEOFMaxRetrys = 5               // how many consecutive early EOFs to allow in grace window
	liveEarlyEOFSleep     = 200 * time.Millisecond
)

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
	if user != "ubugorozi" {
		return errors.New("invalid user")
	}
	if pass != liveSourcePassword {
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

	// Reject if one already active
	if s.liveActive.Load() {
		http.Error(w, "live source already active", http.StatusConflict)
		return
	}

	// Capture metadata
	meta := extractLiveMeta(r)
	s.setLiveMeta(meta)

	var reader io.ReadCloser
	var hijackedConn io.Closer

	// Some clients send Expect: 100-continue before sending body on PUT/POST
	if r.Method != "SOURCE" && strings.EqualFold(r.Header.Get("Expect"), "100-continue") {
		w.WriteHeader(http.StatusContinue)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		log.Printf("[live %s] sent 100-continue for %s", s.ID, r.Method)
	}

	// Decide whether to hijack: always for SOURCE; for PUT/POST if unknown/zero Content-Length to keep raw socket
	if r.Method == "SOURCE" || ((r.Method == http.MethodPut || r.Method == http.MethodPost) && r.ContentLength <= 0) {
		// Some Icecast source clients (e.g. BUTT) use custom METHOD SOURCE and may not set
		// a Content-Length or transfer encoding. Hijack raw connection to read bytes directly.
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack not supported", http.StatusInternalServerError)
			return
		}
		conn, bufRW, err := hj.Hijack()
		if err != nil {
			log.Printf("[live %s] hijack failed: %v", s.ID, err)
			return
		}
		// Send minimal Icecast-like response
		_, _ = bufRW.WriteString("HTTP/1.0 200 OK\r\nServer: Icecast 2.4.0\r\n\r\n")
		_ = bufRW.Flush()
		reader = conn
		hijackedConn = conn
	} else {
		// Regular HTTP methods (PUT/POST) streaming body
		log.Println("=======>Excuted")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		reader = r.Body
	}

	// Mark active
	s.liveMu.Lock()
	s.liveIngest = reader
	s.liveActive.Store(true)
	s.liveMu.Unlock()

	log.Printf("[live %s] connected: method=%s name=%q bitrate=%s", s.ID, r.Method, meta.Name, meta.Bitrate)

	buf := make([]byte, 8192)
	graceStart := time.Now()
	earlyEOFs := 0
	bytesReceived := 0
	receivedAudio := false
	isPutLike := r.Method == http.MethodPut || r.Method == http.MethodPost
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.push(chunk)
			bytesReceived += n
			if !receivedAudio {
				receivedAudio = true
				log.Printf("[live %s] first audio after %s (bytes=%d)", s.ID, time.Since(graceStart).Round(time.Millisecond), bytesReceived)
			}
			if earlyEOFs > 0 {
				earlyEOFs = 0
			}
		}
		if err != nil {
			if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && n == 0 && !receivedAudio {
				// Time based grace only (ignore retry cap) until maxGrace exceeded
				graceElapsed := time.Since(graceStart)
				maxGrace := liveEarlyEOFGrace
				if isPutLike {
					maxGrace = liveEarlyEOFGrace + 10*time.Second
				}
				if graceElapsed < maxGrace {
					earlyEOFs++
					if earlyEOFs%5 == 0 { // log every 5th attempt to reduce noise
						log.Printf("[live %s] waiting for first audio (EOF attempts=%d elapsed=%s grace=%s method=%s)", s.ID, earlyEOFs, graceElapsed.Round(time.Millisecond), maxGrace, r.Method)
					}
					time.Sleep(liveEarlyEOFSleep)
					continue
				}
			}
			// If we reached here: either audio received then read ended, or grace expired without audio
			if !receivedAudio {
				log.Printf("[live %s] terminating: no audio within grace (elapsed=%s attempts=%d method=%s)", s.ID, time.Since(graceStart).Round(time.Millisecond), earlyEOFs, r.Method)
			} else {
				log.Printf("[live %s] READ end n=%d err=%v (totalBytes=%d)", s.ID, n, err, bytesReceived)
			}
			break
		}
	}

	if hijackedConn != nil {
		_ = hijackedConn.Close()
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
