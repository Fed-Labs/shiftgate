package endpoint

import "testing"

func TestNormalizeRemotePeerURLs(t *testing.T) {
	for _, test := range []struct {
		input string
		ok    bool
	}{
		{"https://gpu.example:8443", true},
		{" https://gpu.example:8443/ ", true},
		{"https://[2001:db8::1]:8443", true},
		{"http://gpu.example:8443", false},
		{"tcp://gpu.example:8443", false},
		{"unix:///run/shift.sock", false},
		{"https://", false},
		{"https://user:pass@gpu.example:8443", false},
		{"https://gpu.example:8443/path", false},
		{"https://gpu.example:8443?q=1", false},
		{"https://gpu.example:8443#fragment", false},
		{"https://gpu.example:99999", false},
		{"https://gpu.example:65536", false},
		{"https://gpu.example:0", false},
		{"https://gpu.example:", false},
		{"https://gpu.example:abc", false},
		{"https://gpu.example:+80", false},
		{"https://gpu.example:84430", false},
		{"https://2001:db8::1", false},
		{"https://[2001:db8::1]", true},
		{"https://[::1]:", false},
		{"https://gpu.example:1", true},
		{"https://gpu.example:65535", true},
	} {
		_, err := Normalize(test.input)
		if (err == nil) != test.ok {
			t.Fatalf("Normalize(%q) error = %v, want ok=%v", test.input, err, test.ok)
		}
	}
}
