package routing_test

import (
	"fmt"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

func TestSuggest(t *testing.T) {
	for _, c := range []struct {
		format string
		names  []string
		want   string
	}{
		{"nuget", []string{"Edcom.Common.Utils", "Edcom.Data", "edcom.data", "Tool"}, "Edcom.*:2 Tool:1"},
		{"npm", []string{"@trk/ui", "@trk/api", "left-pad"}, "@trk/*:2 left-pad:1"},
		{"maven", []string{"com.edcom:utils", "com.edcom:core", "org.x:y"}, "com.edcom:2 org.x:1"},
		{"go", []string{"git.example.com/corp/a", "git.example.com/corp/b/v2", "example.com/m"}, "git.example.com/corp:2 example.com/m:1"},
		{"pypi", []string{"edcom-utils", "edcom_core", "solo"}, "edcom-*:1 edcom_*:1 solo:1"},
		{"docker", []string{"team/app", "team/api", "nginx"}, "team/*:2 nginx:1"},
		{"helm", []string{"edcom-api", "edcom-web"}, "edcom-*:2"},
	} {
		got := ""
		for _, s := range routing.Suggest(c.format, c.names) {
			if got != "" {
				got += " "
			}
			got += fmt.Sprintf("%s:%d", s.Pattern, s.Matches)
			for _, ex := range s.Examples {
				if !core.MatchRoutingPattern(c.format, s.Pattern, ex) {
					t.Errorf("%s: pattern %q does not match its example %q", c.format, s.Pattern, ex)
				}
			}
		}
		if got != c.want {
			t.Errorf("Suggest(%s) = %q, want %q", c.format, got, c.want)
		}
	}
	many := make([]string, 0, 80)
	for i := range 80 {
		many = append(many, fmt.Sprintf("p%d", i))
	}
	if n := len(routing.Suggest("rpm", many)); n != routing.MaxSuggestions {
		t.Errorf("capped at %d", n)
	}
}

func TestComponentName(t *testing.T) {
	for _, c := range []struct {
		format string
		comp   core.Component
		want   string
	}{
		{"npm", core.Component{Namespace: "@trk", Name: "ui"}, "@trk/ui"},
		{"npm", core.Component{Namespace: "trk", Name: "ui"}, "@trk/ui"},
		{"maven", core.Component{Namespace: "com.edcom", Name: "utils"}, "com.edcom:utils"},
		{"docker", core.Component{Namespace: "team", Name: "app"}, "team/app"},
		{"nuget", core.Component{Name: "edcom.x"}, "edcom.x"},
	} {
		if got := routing.ComponentName(c.format, &c.comp); got != c.want {
			t.Errorf("ComponentName(%s) = %q, want %q", c.format, got, c.want)
		}
	}
}
