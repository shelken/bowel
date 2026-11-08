package settings

import "testing"

// The address is only checked when the external core is the one in use:
// switching back to the built-in core keeps what was typed.
func TestExternalCoreValidate(t *testing.T) {
	for _, c := range []struct {
		source, addr string
		bad          bool
	}{
		{"", "", false},
		{"", "junk", false},
		{"external", "127.0.0.1:9090", false},
		{"external", "localhost:9090", false},
		{"external", "", true},
		{"external", "9090", true},
		{"external", "http://127.0.0.1:9090", true},
		{"external", "127.0.0.1:0", true},
		{"external", "127.0.0.1:99999", true},
		{"cloud", "127.0.0.1:9090", true},
	} {
		s := Defaults.Clone()
		s.CoreSource, s.CoreAddr = c.source, c.addr
		if err := s.Validate(); (err != nil) != c.bad {
			t.Errorf("source %q addr %q: error = %v, want error: %v", c.source, c.addr, err, c.bad)
		}
	}
}
