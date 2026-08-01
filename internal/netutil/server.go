package netutil

import (
	"encoding/json"
	"net/http"
	"strings"
)

type ServerRes struct {
	Message  string `json:"message,omitempty"`
	ErrorMsg string `json:"error_msg,omitempty"`
	Data     any    `json:"data,omitempty"`
	Success  bool   `json:"success"`
}

func NewServerRes() *ServerRes {
	return &ServerRes{
		Success: false,
	}
}

func ServerResponse(w http.ResponseWriter, code int, message string, data any) {
	sr := NewServerRes()
	if code < 400 {
		sr.Success = true
		sr.Message = message
		sr.Data = data
	} else {
		sr.ErrorMsg = message
	}
	w.WriteHeader(code)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sr)
}

// WithCORS applies CORS headers and handles preflight requests.
// allowedOrigins is a comma-separated list, e.g. "https://app.example.com,https://admin.example.com"
// Use "*" to allow all origins.
func WithCORS(next http.HandlerFunc, allowedOrigins string) http.HandlerFunc {
	origins := parseAllowedOrigins(allowedOrigins)

	return func(w http.ResponseWriter, r *http.Request) {
		applyCORSHeaders(w, r, origins)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

func parseAllowedOrigins(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{""}
	}

	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, part := range parts {
		v := strings.TrimSpace(part)
		if v != "" {
			origins = append(origins, v)
		}
	}

	if len(origins) == 0 {
		return []string{""}
	}
	return origins
}

func applyCORSHeaders(w http.ResponseWriter, r *http.Request, origins []string) {
	origin := r.Header.Get("Origin")
	allowedOrigin := ""

	for _, allowed := range origins {
		if allowed == "" {
			break
		}
		if origin != "" && strings.EqualFold(strings.TrimSpace(allowed), origin) {
			allowedOrigin = origin
			break
		}
	}

	if allowedOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Vary", "Origin")
	}

	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Range")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
	w.Header().Set("Access-Control-Max-Age", "86400")
}
