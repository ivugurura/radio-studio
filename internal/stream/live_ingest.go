package stream

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type LiveMeta struct {
	Name        string
	Genre       string
	Description string
	URL         string
	Bitrate     string
	SampleRate  string
	Channels    string
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

// Recommended encoder settings for seamless switching with AutoDJ: MP3,
// 48kHz, stereo, 128kbps CBR — must match the library (see DEFAULT_SR_HZ).

// checkIcecastAuth validates Basic Auth against this studio's current credentials.
func (s *Studio) checkIcecastAuth(r *http.Request) error {
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
	expectedUser, expectedPass := s.credentials()
	if expectedUser == "" || expectedPass == "" {
		return errors.New("studio credentials not yet loaded")
	}
	if user != expectedUser {
		return errors.New("invalid user")
	}
	if pass != expectedPass {
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
	if err := s.checkIcecastAuth(r); err != nil {
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
	logFormatCheck(s.ID, s.bitrateKbps, s.srHz, s.ch, meta)

	// Reject a mismatched encoder outright: a format mismatch breaks playback
	// for listeners at the AutoDJ/live splice point (VLC and browsers alike).
	if err := validateLiveBitrate(meta.Bitrate, s.bitrateKbps); err != nil {
		log.Printf("[live %s] rejected: %v", s.ID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateLiveSampleRate(meta.SampleRate, s.srHz); err != nil {
		log.Printf("[live %s] rejected: %v", s.ID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateLiveChannels(meta.Channels, s.ch); err != nil {
		log.Printf("[live %s] rejected: %v", s.ID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

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

	log.Printf("[live %s] connected: method=%s name=%q bitrate=%s samplerate=%s channels=%s", s.ID, r.Method, meta.Name, meta.Bitrate, meta.SampleRate, meta.Channels)

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

// parseIceAudioInfo parses Ice-Audio-Info's "key=value;..." pairs, stripping
// an optional "ice-" key prefix (some encoders send "ice-bitrate", not "bitrate").
func parseIceAudioInfo(raw string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(raw, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		key := strings.TrimPrefix(strings.ToLower(kv[0]), "ice-")
		out[key] = kv[1]
	}
	return out
}

// Live metadata helpers
func extractLiveMeta(r *http.Request) LiveMeta {
	audioInfo := parseIceAudioInfo(r.Header.Get("Ice-Audio-Info"))

	// Resolve bitrate: Ice-Bitrate (BUTT), Icy-Br (ffmpeg), or inside Ice-Audio-Info.
	bitrate := r.Header.Get("Ice-Bitrate")
	if bitrate == "" {
		bitrate = r.Header.Get("Icy-Br")
	}
	if bitrate == "" {
		bitrate = audioInfo["bitrate"]
	}

	lm := LiveMeta{
		Name:        r.Header.Get("Ice-Name"),
		Genre:       r.Header.Get("Ice-Genre"),
		Description: r.Header.Get("Ice-Description"),
		URL:         r.Header.Get("Ice-URL"),
		Bitrate:     bitrate,
		SampleRate:  audioInfo["samplerate"],
		Channels:    audioInfo["channels"],
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

// parseNumericHeader extracts a leading numeric value, tolerating suffixes like "128kb/s".
func parseNumericHeader(raw string) (int, bool) {
	digits := strings.TrimFunc(raw, func(r rune) bool { return r < '0' || r > '9' })
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

func matchLabel(got int, ok bool, expected int) string {
	if !ok {
		return "unknown"
	}
	if got == expected {
		return "false"
	}
	return "true"
}

// logFormatCheck logs the encoder's declared format against what the studio expects.
func logFormatCheck(studioID string, expectedKbps, expectedHz, expectedCh int, meta LiveMeta) {
	receivedKbps, kbpsOK := parseNumericHeader(meta.Bitrate)
	receivedHz, hzOK := parseNumericHeader(meta.SampleRate)
	receivedCh, chOK := parseNumericHeader(meta.Channels)
	log.Printf("[live %s] FORMAT-CHECK expected_bitrate=%dkbps received_bitrate=%q bitrate_mismatch=%s expected_samplerate=%dHz received_samplerate=%q samplerate_mismatch=%s expected_channels=%d received_channels=%q channels_mismatch=%s",
		studioID, expectedKbps, meta.Bitrate, matchLabel(receivedKbps, kbpsOK, expectedKbps),
		expectedHz, meta.SampleRate, matchLabel(receivedHz, hzOK, expectedHz),
		expectedCh, meta.Channels, matchLabel(receivedCh, chOK, expectedCh))
}

// validateLiveBitrate rejects a connection whose bitrate is missing or doesn't match expectedKbps.
func validateLiveBitrate(rawBitrate string, expectedKbps int) error {
	n, ok := parseNumericHeader(rawBitrate)
	if !ok {
		return fmt.Errorf("missing or unparseable live bitrate (studio expects %dkbps; encoder must send Ice-Bitrate, Icy-Br, or Ice-Audio-Info)", expectedKbps)
	}
	if n != expectedKbps {
		return fmt.Errorf("live bitrate mismatch: encoder sent %dkbps, studio expects %dkbps", n, expectedKbps)
	}
	return nil
}

// validateLiveSampleRate rejects a connection whose sample rate is missing or doesn't match expectedHz.
func validateLiveSampleRate(rawSampleRate string, expectedHz int) error {
	n, ok := parseNumericHeader(rawSampleRate)
	if !ok {
		return fmt.Errorf("missing or unparseable live sample rate (studio expects %dHz; encoder must send Ice-Audio-Info with a samplerate field)", expectedHz)
	}
	if n != expectedHz {
		return fmt.Errorf("live sample rate mismatch: encoder sent %dHz, studio expects %dHz", n, expectedHz)
	}
	return nil
}

// validateLiveChannels rejects a connection whose channel count is missing or doesn't match expectedCh.
func validateLiveChannels(rawChannels string, expectedCh int) error {
	n, ok := parseNumericHeader(rawChannels)
	if !ok {
		return fmt.Errorf("missing or unparseable live channel count (studio expects %d; encoder must send Ice-Audio-Info with a channels field)", expectedCh)
	}
	if n != expectedCh {
		return fmt.Errorf("live channel count mismatch: encoder sent %d, studio expects %d", n, expectedCh)
	}
	return nil
}
