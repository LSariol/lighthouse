package release

import (
	"testing"

	"github.com/lsariol/lighthouse/internal/github"
)

func TestParse(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "1.2.3", "v0.0.1", "v10.20.30"} {
		if _, ok := Parse(tag); !ok {
			t.Errorf("%s isn't a release", tag)
		}
	}
	for _, tag := range []string{"v1.1.0-rc.1", "latest", "v1.2", "v01.2.3", "release-1.2.3", "v1.2.3+build", ""} {
		if _, ok := Parse(tag); ok {
			t.Errorf("%s is a release", tag)
		}
	}
}

func TestHigher(t *testing.T) {
	for _, c := range [][3]string{{"v1.2.0", "v1.10.0", "v1.10.0"}, {"v1.2.0", "v1.0.0", "v1.2.0"}, {"", "v1.0.0", "v1.0.0"},
		{"v1.0.0", "", "v1.0.0"}, {"", "", ""}, {"latest", "v0.1.0", "v0.1.0"}} {
		if got := Higher(c[0], c[1]); got != c[2] {
			t.Errorf("Higher(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestNewest(t *testing.T) {
	tags := []github.Tag{{Name: "latest", SHA: "x"}, {Name: "v1.9.0", SHA: "a"}, {Name: "v1.10.0", SHA: "b"}, {Name: "v2.0.0-rc.1", SHA: "c"}, {Name: "1.10.0", SHA: "d"}}
	tag, v, ok := Newest(tags)
	if !ok || tag.Name != "v1.10.0" || tag.SHA != "b" || v.Minor != 10 {
		t.Errorf("Newest = %+v, %+v, %v", tag, v, ok)
	}
	if _, _, ok := Newest([]github.Tag{{Name: "latest", SHA: "x"}}); ok {
		t.Error("a release among no releases")
	}
	if tag, ok := Find(tags, "v1.9.0"); !ok || tag.SHA != "a" {
		t.Errorf("Find = %+v, %v", tag, ok)
	}
}
