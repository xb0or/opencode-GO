package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"1.0.0", "v1.0.0", 0},
		{"v1.2.3", "v1.2.4", -1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.0.0", "v1.0.0-rc1", 0},   // pre-release suffix ignored
		{"v1.0.0+meta", "v1.0.0", 0},  // build metadata ignored
		{"garbage", "v1.0.0", -1},      // fallback string compare
	}
	for _, c := range cases {
		got := Compare(c.a, c.b)
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestGitHubURL(t *testing.T) {
	if got := GitHubURL(); got != "https://github.com/"+Repo {
		t.Errorf("GitHubURL() = %q, want https://github.com/%s", got, Repo)
	}
}