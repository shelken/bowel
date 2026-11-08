package locate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oboo/moho/internal/geo"
)

const sampleTrace = `fl=12f345
h=www.cloudflare.com
ip=203.0.113.7
ts=1762500000.123
visit_scheme=https
uag=Go-http-client/1.1
colo=NRT
sliver=none
http=http/1.1
loc=JP
tls=TLSv1.3
sni=plaintext
warp=off
gateway=off
rbi=off
kex=X25519
`

func TestParseTrace(t *testing.T) {
	ip, loc, colo := parseTrace(sampleTrace)
	if ip != "203.0.113.7" || loc != "JP" || colo != "NRT" {
		t.Fatalf("got %q %q %q", ip, loc, colo)
	}
	ip, loc, colo = parseTrace("ip=2001:db8::1\r\nloc=HK\r\ncolo=HKG\r\n")
	if ip != "2001:db8::1" || loc != "HK" || colo != "HKG" {
		t.Fatalf("CRLF: got %q %q %q", ip, loc, colo)
	}
	if ip, loc, colo = parseTrace("<html>nope</html>"); ip+loc+colo != "" {
		t.Fatalf("garbage: got %q %q %q", ip, loc, colo)
	}
}

func TestGuess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "locate.json")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	hk, _ := geo.FromTrace("HK", "HKG")

	l := New(path)
	l.now = func() time.Time { return now }
	l.mu.Lock()
	l.cache["fresh"] = Result{Place: &hk, Source: SourceProbe, IP: "198.51.100.1", At: now.Add(-23 * time.Hour)}
	l.cache["stale"] = Result{Place: &hk, Source: SourceProbe, At: now.Add(-25 * time.Hour)}
	l.cache["noplace"] = Result{Source: SourceProbe, IP: "198.51.100.2", At: now}
	if err := l.saveLocked(); err != nil {
		t.Fatal(err)
	}
	l.mu.Unlock()

	// A new locator loads what the first one saved.
	l = New(path)
	l.now = func() time.Time { return now }

	if r := l.Guess("fresh", "🇯🇵 日本 01"); r.Source != SourceProbe || r.Place == nil || r.Place.Country != "HK" || r.IP != "198.51.100.1" {
		t.Errorf("fresh probe: %+v", r)
	}
	if r := l.Guess("stale", "🇯🇵 日本 01"); r.Source != SourceName || r.Place == nil || r.Place.Country != "JP" {
		t.Errorf("stale probe → name: %+v", r)
	}
	// A node whose name says nothing keeps the place it was measured at.
	if r := l.Guess("stale", "Relay-7 专线"); r.Source != SourceProbe || r.Place == nil || r.Place.Country != "HK" {
		t.Errorf("stale probe, no name: %+v", r)
	}
	if r := l.Guess("noplace", "US Los Angeles"); r.Source != SourceName || r.Place == nil || r.Place.Country != "US" {
		t.Errorf("probe without place → name: %+v", r)
	}
	if r := l.Guess("other", "Relay-7 专线"); r.Source != SourceUnknown || r.Place != nil {
		t.Errorf("unknown: %+v", r)
	}
	if r := l.Guess("other", "新加坡 03"); r.Source != SourceName || r.Place.Country != "SG" {
		t.Errorf("name: %+v", r)
	}

	// A missing or broken cache file is an empty cache.
	_ = os.WriteFile(path, []byte("{"), 0o600)
	if r := New(path).Guess("fresh", ""); r.Source != SourceUnknown {
		t.Errorf("broken cache: %+v", r)
	}
	if r := New("").Guess("fresh", ""); r.Source != SourceUnknown {
		t.Errorf("no cache: %+v", r)
	}
}
