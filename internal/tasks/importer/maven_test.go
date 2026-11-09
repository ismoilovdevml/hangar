package importer

import (
	"crypto/sha1" //nolint:gosec // maven checksum files
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// mavenSource serves a Maven 2 layout with HTML directory listings (when
// listings is set) and .sha1 files.
func mavenSource(t *testing.T, listings bool, artifacts map[string][]string) *fakeSource {
	t.Helper()
	src := newFakeSource(t)
	dirs := map[string]map[string]bool{}
	addDir := func(path string) {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		for i := range parts {
			parent := "/" + strings.Join(parts[:i], "/")
			if i > 0 {
				parent += "/"
			}
			if dirs[parent] == nil {
				dirs[parent] = map[string]bool{}
			}
			child := parts[i]
			if i < len(parts)-1 || strings.HasSuffix(path, "/") {
				child += "/"
			}
			dirs[parent][child] = true
		}
	}
	file := func(path string, body []byte, at time.Time) {
		src.mu.Lock()
		src.files[path] = fakeFile{body: body, modified: at}
		src.mu.Unlock()
		sum := sha1.Sum(body) //nolint:gosec // maven checksum
		src.add(path+".sha1", []byte(hex.EncodeToString(sum[:])), "text/plain")
		addDir(path)
	}
	for ga, versions := range artifacts {
		g, a, _ := strings.Cut(ga, ":")
		base := "/" + strings.ReplaceAll(g, ".", "/") + "/" + a + "/"
		var vs strings.Builder
		for i, v := range versions {
			at := time.Date(2018, 6, 1+i, 12, 0, 0, 0, time.UTC)
			pom := fmt.Sprintf(`<project><modelVersion>4.0.0</modelVersion><groupId>%s</groupId><artifactId>%s</artifactId><version>%s</version><packaging>jar</packaging></project>`, g, a, v)
			file(base+v+"/"+a+"-"+v+".pom", []byte(pom), at)
			file(base+v+"/"+a+"-"+v+".jar", zipOf(t, map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n", "Lib.class": v}), at)
			file(base+v+"/"+a+"-"+v+"-sources.jar", zipOf(t, map[string]string{"Lib.java": v}), at)
			fmt.Fprintf(&vs, "<version>%s</version>", v)
		}
		file(base+"maven-metadata.xml", []byte("<metadata><groupId>"+g+"</groupId><artifactId>"+a+"</artifactId><versioning><versions>"+vs.String()+"</versions></versioning></metadata>"), time.Time{})
	}
	if listings {
		for dir, entries := range dirs {
			var b strings.Builder
			b.WriteString(`<html><body><a href="../">../</a>`)
			for _, e := range sortedKeys(map[string]string(func() map[string]string {
				m := map[string]string{}
				for k := range entries {
					m[k] = ""
				}
				return m
			}())) {
				fmt.Fprintf(&b, `<a href="%s">%s</a>`, e, e)
			}
			b.WriteString("</body></html>")
			src.add(dir, []byte(b.String()), "text/html")
		}
	}
	return src
}

func TestImportMaven(t *testing.T) {
	h := newHangar(t)
	h.repo(t, "maven-releases", "maven", core.RepoHosted, "")
	src := mavenSource(t, true, map[string][]string{"com.acme:core": {"1.0", "1.1"}, "com.acme.tools:cli": {"2.0"}, "org.other:x": {"1.0"}})
	cfg := ImportConfig{Repository: "maven-releases", Source: Source{URL: src.URL()}, Filters: Filters{Packages: []string{"com.acme*:*"}}}
	rep, res, err := h.runImport(t, cfg)
	if err != nil || res.Imported != 3 || res.Packages != 2 {
		t.Fatalf("import: %+v %v\n%s", res, err, rep.log())
	}
	md := httpGet(t, h.url+"/repository/maven-releases/com/acme/core/maven-metadata.xml")
	if !strings.Contains(md, "<version>1.1</version>") {
		t.Fatalf("metadata:\n%s", md)
	}
	httpGet(t, h.url+"/repository/maven-releases/com/acme/core/1.1/core-1.1-sources.jar")
	repo, _ := h.env.Repos.Get(t.Context(), "maven-releases")
	if c, _ := h.env.Components.GetComponent(t.Context(), repo.ID, "com.acme", "core", "1.0"); c == nil || !c.CreatedAt.Equal(time.Date(2018, 6, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("component %+v", c)
	}
	if _, res, err = h.runImport(t, cfg); err != nil || res.Skipped != 3 {
		t.Fatalf("second run: %+v %v", res, err)
	}

	// Without listings: patterns fail, exact coordinates work from the POM.
	plain := mavenSource(t, false, map[string][]string{"org.plain:lib": {"3.0"}})
	if _, _, err := h.runImport(t, ImportConfig{Repository: "maven-releases", Source: Source{URL: plain.URL()},
		Filters: Filters{Packages: []string{"org.plain:*"}}}); err == nil {
		t.Fatal("pattern without listings succeeded")
	}
	rep, res, err = h.runImport(t, ImportConfig{Repository: "maven-releases", Source: Source{URL: plain.URL()},
		Filters: Filters{Packages: []string{"org.plain:lib"}}})
	if err != nil || res.Imported != 1 {
		t.Fatalf("exact import: %+v %v\n%s", res, err, rep.log())
	}
	httpGet(t, h.url+"/repository/maven-releases/org/plain/lib/3.0/lib-3.0-sources.jar")
}
