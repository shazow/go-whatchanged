package release

import (
	"testing"

	"github.com/shazow/go-whatchanged/internal/render"
)

func TestTagsFor(t *testing.T) {
	tests := []struct {
		modPath, dir string
		want         Tags
	}{
		{"example.com/m", "", Tags{}},
		{"example.com/m/sub", "sub", Tags{Prefix: "sub/"}},
		{"example.com/m/a/b", "a/b", Tags{Prefix: "a/b/"}},
		{"example.com/m/v2", "", Tags{Major: "/v2"}},
		{"example.com/m/v2", "v2", Tags{Major: "/v2"}},
		{"example.com/m/sub/v3", "sub", Tags{Prefix: "sub/", Major: "/v3"}},
		{"example.com/m/sub/v3", "sub/v3", Tags{Prefix: "sub/", Major: "/v3"}},
		{"gopkg.in/yaml.v3", "", Tags{Major: ".v3"}},
	}
	for _, tc := range tests {
		got, err := TagsFor(tc.modPath, tc.dir)
		if err != nil {
			t.Errorf("TagsFor(%q, %q): %v", tc.modPath, tc.dir, err)
			continue
		}
		if got != tc.want {
			t.Errorf("TagsFor(%q, %q) = %+v, want %+v", tc.modPath, tc.dir, got, tc.want)
		}
	}
	if _, err := TagsFor("example.com/m/v2.1", ""); err == nil {
		t.Error("TagsFor accepted an invalid module path")
	}
}

func TestVersion(t *testing.T) {
	tests := []struct {
		tags Tags
		tag  string
		want string
	}{
		{Tags{}, "v1.2.3", "v1.2.3"},
		{Tags{}, "v0.0.1", "v0.0.1"},
		{Tags{}, "v1.5.0-rc.1", "v1.5.0-rc.1"},
		{Tags{}, "v1.2", ""},
		{Tags{}, "1.2.3", ""},
		{Tags{}, "v1.2.3+incompatible", ""},
		{Tags{}, "release-1", ""},
		{Tags{}, "v2.0.0", ""}, // needs a /v2 module path
		{Tags{}, "sub/v1.2.3", ""},
		{Tags{Prefix: "sub/"}, "sub/v1.2.3", "v1.2.3"},
		{Tags{Prefix: "sub/"}, "v1.2.3", ""},
		{Tags{Prefix: "sub/"}, "other/v1.2.3", ""},
		{Tags{Major: "/v2"}, "v2.0.0", "v2.0.0"},
		{Tags{Major: "/v2"}, "v2.1.0-beta.2", "v2.1.0-beta.2"},
		{Tags{Major: "/v2"}, "v1.9.9", ""},
		{Tags{Major: "/v2"}, "v3.0.0", ""},
		{Tags{Prefix: "sub/", Major: "/v3"}, "sub/v3.0.1", "v3.0.1"},
		{Tags{Major: ".v3"}, "v3.0.1", "v3.0.1"},
	}
	for _, tc := range tests {
		if got := tc.tags.Version(tc.tag); got != tc.want {
			t.Errorf("%+v.Version(%q) = %q, want %q", tc.tags, tc.tag, got, tc.want)
		}
	}
}

func TestExample(t *testing.T) {
	tests := []struct {
		tags Tags
		want string
	}{
		{Tags{}, "v1.2.3"},
		{Tags{Prefix: "sub/"}, "sub/v1.2.3"},
		{Tags{Major: "/v2"}, "v2.2.3"},
		{Tags{Prefix: "sub/", Major: "/v12"}, "sub/v12.2.3"},
		{Tags{Major: ".v3"}, "v3.2.3"},
	}
	for _, tc := range tests {
		if got := tc.tags.Example(); got != tc.want {
			t.Errorf("%+v.Example() = %q, want %q", tc.tags, got, tc.want)
		}
	}
}

func TestNext(t *testing.T) {
	tests := []struct {
		v    string
		lvl  render.Level
		want string
	}{
		{"v1.4.0", render.Major, "v2.0.0"},
		{"v1.4.0", render.Minor, "v1.5.0"},
		{"v1.4.0", render.Patch, "v1.4.1"},
		{"v1.4.7", render.Major, "v2.0.0"},
		{"v1.4.7", render.Minor, "v1.5.0"},
		{"v1.4.7", render.Patch, "v1.4.8"},
		{"v0.3.2", render.Major, "v0.4.0"},
		{"v0.3.2", render.Minor, "v0.4.0"},
		{"v0.3.2", render.Patch, "v0.3.3"},
		{"v12.0.0", render.Major, "v13.0.0"},
		{"v1.5.0-rc.1", render.Major, "v1.5.0"},
		{"v1.5.0-rc.1", render.Minor, "v1.5.0"},
		{"v1.5.0-rc.1", render.Patch, "v1.5.0"},
		{"v2.0.0-beta.3", render.Major, "v2.0.0"},
		{"v1.2", render.Patch, ""},
		{"garbage", render.Patch, ""},
	}
	for _, tc := range tests {
		if got := Next(tc.v, tc.lvl); got != tc.want {
			t.Errorf("Next(%q, %v) = %q, want %q", tc.v, tc.lvl, got, tc.want)
		}
	}
}
