package supervisor

import "testing"

// A production gc must not be mistaken for a Go test binary because a
// directory in its install path happens to contain ".test" (gcf-a14q): only
// the binary's own name decides.
func TestIsTestBinaryArg0MatchesOnlyTheBinaryName(t *testing.T) {
	cases := []struct {
		name  string
		argv0 string
		want  bool
	}{
		{"compiled test binary", "/tmp/go-build123/b001/supervisor.test", true},
		{"compiled test binary, relative", "./gc.test", true},
		{"production gc", "/usr/local/bin/gc", false},
		{"dir named app.test", "/home/u/app.test/bin/gc", false},
		{"dir named .testing", "/home/u/.testing/bin/gc", false},
		{"dir ends in .test", "/home/u/proj.test/gc", false},
		{"binary name has .test infix", "/usr/bin/gc.test-tools", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTestBinaryArg0(c.argv0); got != c.want {
				t.Errorf("isTestBinaryArg0(%q) = %v, want %v", c.argv0, got, c.want)
			}
		})
	}
}
