package update

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in     string
		want   version
		wantOK bool
	}{
		{"v1.2.3", version{1, 2, 3, "", true}, true},
		{"1.2.3", version{1, 2, 3, "", true}, true},
		{"v0.1.0", version{0, 1, 0, "", true}, true},
		{"v0.2.0-rc1", version{0, 2, 0, "rc1", true}, true},
		{"v0.1.0-3-gabc123-dirty", version{0, 1, 0, "3-gabc123-dirty", true}, true},
		{"v10.20.30", version{10, 20, 30, "", true}, true},
		{"", version{}, false},
		{"dev", version{}, false},
		{"v1.2", version{}, false},
		{"v1.2.3.4", version{}, false},
		{"v-1.2.3", version{}, false},
		{"abc", version{}, false},
	}
	for _, c := range cases {
		got := parseVersion(c.in)
		if got.valid != c.wantOK || got != c.want {
			t.Errorf("parseVersion(%q) = %+v,%v; want %+v,%v", c.in, got, got.valid, c.want, c.wantOK)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.2.0", -1},
		{"v0.2.0", "v0.1.0", 1},
		{"v0.2.0", "v0.2.0", 0},
		{"v0.1.9", "v0.2.0", -1},
		{"v0.2.0", "v0.10.0", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.2.0-rc1", "v0.2.0", 0},    // suffix ignored
		{"v0.2.0-3-gabc", "v0.1.9", 1}, // dev build above the last release
		{"dev", "v1.0.0", 0},           // unparseable never compares
		{"v1.0.0", "dev", 0},
	}
	for _, c := range cases {
		got := compare(parseVersion(c.a), parseVersion(c.b))
		if got != c.want {
			t.Errorf("compare(%q, %q) = %d; want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewerAvailable(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.1.0-3-gabc123-dirty", "v0.2.0", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.2.0", "v0.2.0", false},
		{"v0.2.0-rc1", "v0.2.0-rc2", false}, // same numeric triple: not newer
		{"dev", "v0.9.0", false},
	}
	for _, c := range cases {
		got := NewerAvailable(c.current, c.latest)
		if got != c.want {
			t.Errorf("NewerAvailable(%q, %q) = %v; want %v", c.current, c.latest, got, c.want)
		}
	}
}
