// Package release picks releases: plain version tags (v1.2.3), compared as numbers.
package release

import (
	"regexp"
	"strconv"

	"github.com/lsariol/lighthouse/internal/github"
)

// Version is a release: its tag as written, and its numbers.
type Version struct {
	Tag                 string
	Major, Minor, Patch int
}

var plain = regexp.MustCompile(`^v?(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})$`)

// Parse reads a tag; ok is false if it isn't a release.
func Parse(tag string) (Version, bool) {
	m := plain.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	v := Version{Tag: tag}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, true
}

// Newer reports whether v is a later release than w.
func (v Version) Newer(w Version) bool {
	if v.Major != w.Major {
		return v.Major > w.Major
	}
	if v.Minor != w.Minor {
		return v.Minor > w.Minor
	}
	return v.Patch > w.Patch
}

// Newest returns the newest release among tags.
func Newest(tags []github.Tag) (github.Tag, Version, bool) {
	var best github.Tag
	var bestV Version
	found := false
	for _, t := range tags {
		v, ok := Parse(t.Name)
		if ok && (!found || v.Newer(bestV)) {
			best, bestV, found = t, v, true
		}
	}
	return best, bestV, found
}

// Higher returns whichever of two versions is the higher release: a, b, or,
// when neither is a release, "".
func Higher(a string, b string) string {
	va, okA := Parse(a)
	vb, okB := Parse(b)
	switch {
	case okA && okB && vb.Newer(va), !okA && okB:
		return b
	case okA:
		return a
	}
	return ""
}

// Find returns the tag called name.
func Find(tags []github.Tag, name string) (github.Tag, bool) {
	for _, t := range tags {
		if t.Name == name {
			return t, true
		}
	}
	return github.Tag{}, false
}
