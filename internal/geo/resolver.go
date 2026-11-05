package geo

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"

	"github.com/ivugurura/radio-studio/internal/listeners"
	"github.com/oschwald/geoip2-golang"
)

type Resolver struct {
	db   *geoip2.Reader
	salt []byte
	ok   bool
}

func NewResolver(dbPath string, salt string, enabled bool) *Resolver {
	r := &Resolver{
		salt: []byte(salt),
	}
	if !enabled {
		return r
	}
	db, err := geoip2.Open(dbPath)
	if err != nil {
		log.Printf("GeoIP: failed opening db: %v (continuing without geo)", err)
		return r
	}
	r.db = db
	r.ok = true
	return r
}

func (r *Resolver) hash(l *listeners.Listener) {
	if l.RemoteIP == nil {
		return
	}
	sum := sha256.Sum256(append(r.salt, []byte(l.RemoteIP.String())...))
	l.IPHash = hex.EncodeToString(sum[:])
	l.RemoteIP = nil // drop raw IP
}

// Fingerprint returns a salted hash of a client's IP and User-Agent. It lets
// the studio recognise a returning player that sends no listener id, without
// keeping the raw IP.
func (r *Resolver) Fingerprint(ip net.IP, userAgent string) string {
	h := sha256.New()
	h.Write(r.salt)
	h.Write([]byte(ip.String()))
	h.Write([]byte{0})
	h.Write([]byte(userAgent))
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Resolver) Close() {
	if r.db != nil {
		r.db.Close()
	}
}

func (r *Resolver) Enrich(l *listeners.Listener) {
	if !r.ok || l.RemoteIP == nil {
		r.hash(l)
		return
	}
	city, err := r.db.City(l.RemoteIP)
	if err != nil {
		r.hash(l)
		return
	}
	if city.Country.IsoCode != "" {
		l.Country = city.Country.IsoCode
	}
	if len(city.Subdivisions) > 0 {
		l.Region = city.Subdivisions[0].Names["en"]
	}
	if city.City.Names["en"] != "" {
		l.City = city.City.Names["en"]
	}
	l.Lat = round2(city.Location.Latitude)
	l.Lon = round2(city.Location.Longitude)
	r.hash(l)
	l.Enriched.Store(true)
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
