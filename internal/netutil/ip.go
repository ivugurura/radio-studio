package netutil

import (
	"net"
	"net/http"
	"strings"
)

func ExtractClientIp(r *http.Request) net.IP {
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.SplitSeq(xff, ",")
		for p := range parts {
			ip := net.ParseIP(strings.TrimSpace(p))
			if ip != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip
		}
	}
	return nil
}

func ClassifyUserAgent(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "vlc"):
		return "vlc"
	case strings.Contains(l, "winamp"):
		return "winamp"
	case strings.Contains(l, "android"):
		return "android_browser"
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad"):
		return "ios_browser"
	case strings.Contains(l, "mozilla"):
		return "browser"
	default:
		return "other"
	}
}
