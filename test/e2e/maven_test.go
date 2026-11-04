//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Maven acceptance (docs/FORMATS.md, "Maven"): `mvn deploy` of a release and
// two SNAPSHOT builds into hosted repositories, a rejected release redeploy
// (allow_once), `mvn install` and `mvn dependency:get` resolving through a
// group of hosted + Maven Central proxy, `gradle publish`, and `gradle build`
// resolving through the group.
//
// Needs HANGAR_E2E_URL, HANGAR_E2E_TOKEN (a token allowed to create
// repositories; it is also the Basic password for mvn and gradle), and mvn
// (gradle optional) on PATH. HANGAR_E2E_BLOB_STORE defaults to "default".
// Maven Central must be reachable from the server.

const mavenE2ELibPOM = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>{{GROUP}}</groupId>
  <artifactId>greeter</artifactId>
  <version>{{VERSION}}</version>
  <packaging>jar</packaging>
  <properties>
    <maven.compiler.release>17</maven.compiler.release>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
  </properties>
  <distributionManagement>
    <repository><id>hangar</id><url>{{RELEASES}}</url></repository>
    <snapshotRepository><id>hangar</id><url>{{SNAPSHOTS}}</url></snapshotRepository>
  </distributionManagement>
</project>
`

const mavenE2EAppPOM = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>{{GROUP}}</groupId>
  <artifactId>app</artifactId>
  <version>1.0</version>
  <properties>
    <maven.compiler.release>17</maven.compiler.release>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
  </properties>
  <dependencies>
    <dependency><groupId>{{GROUP}}</groupId><artifactId>greeter</artifactId><version>1.0</version></dependency>
    <dependency><groupId>org.apache.commons</groupId><artifactId>commons-lang3</artifactId><version>3.17.0</version></dependency>
  </dependencies>
</project>
`

// mavenE2ESettings sends credentials for server id "hangar", routes every
// repository (including Maven Central) through the group, and enables
// snapshots for the mirrored central so SNAPSHOT dependencies resolve.
const mavenE2ESettings = `<settings>
  <servers>
    <server><id>hangar</id><username>e2e</username><password>${env.HANGAR_E2E_TOKEN}</password></server>
  </servers>
  <mirrors>
    <mirror><id>hangar</id><mirrorOf>*</mirrorOf><url>{{GROUPURL}}</url></mirror>
  </mirrors>
  <profiles>
    <profile>
      <id>hangar-snapshots</id>
      <repositories>
        <repository><id>central</id><url>http://central</url>
          <releases><enabled>true</enabled></releases><snapshots><enabled>true</enabled></snapshots></repository>
      </repositories>
      <pluginRepositories>
        <pluginRepository><id>central</id><url>http://central</url>
          <releases><enabled>true</enabled></releases><snapshots><enabled>true</enabled></snapshots></pluginRepository>
      </pluginRepositories>
    </profile>
  </profiles>
  <activeProfiles><activeProfile>hangar-snapshots</activeProfile></activeProfiles>
</settings>
`

type mavenE2E struct {
	t                *testing.T
	base, token      string
	work, settings   string
	releases, snaps  string
	proxy, group     string
	groupID          string
	mvn, gradle      string
	repoURL          func(name string) string
	localRepoCounter int
}

func TestMaven(t *testing.T) {
	base := baseURL(t)
	token := os.Getenv("HANGAR_E2E_TOKEN")
	if token == "" {
		t.Skip("HANGAR_E2E_TOKEN not set")
	}
	mvn, _ := exec.LookPath("mvn")
	gradle, _ := exec.LookPath("gradle")
	if mvn == "" && gradle == "" {
		t.Skip("neither mvn nor gradle installed")
	}
	store := os.Getenv("HANGAR_E2E_BLOB_STORE")
	if store == "" {
		store = "default"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	e := &mavenE2E{t: t, base: base, token: token, work: t.TempDir(), mvn: mvn, gradle: gradle,
		releases: "maven-e2e-rel-" + suffix, snaps: "maven-e2e-snap-" + suffix,
		proxy: "maven-e2e-central-" + suffix, group: "maven-e2e-public-" + suffix,
		groupID: "com.acme.e2e" + suffix}
	e.repoURL = func(name string) string { return base + "/repository/" + name + "/" }

	for _, r := range []map[string]any{
		{"name": e.releases, "format": "maven", "type": "hosted", "blob_store": store, "config": map[string]any{
			"hosted": map[string]any{"write_policy": "allow_once"}, "format": map[string]any{"version_policy": "release"}}},
		{"name": e.snaps, "format": "maven", "type": "hosted", "blob_store": store, "config": map[string]any{
			"hosted": map[string]any{"write_policy": "allow"}, "format": map[string]any{"version_policy": "snapshot"}}},
		{"name": e.proxy, "format": "maven", "type": "proxy", "blob_store": store, "config": map[string]any{
			"proxy":  map[string]any{"remote_url": "https://repo1.maven.org/maven2/"},
			"format": map[string]any{"version_policy": "release"}}},
		{"name": e.group, "format": "maven", "type": "group", "config": map[string]any{
			"group": map[string]any{"members": []string{e.releases, e.snaps, e.proxy}}}},
	} {
		if code, body := e.api(http.MethodPost, "/api/v1/repositories", r); code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", r["name"], code, body)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{e.group, e.proxy, e.snaps, e.releases} {
			e.api(http.MethodDelete, "/api/v1/repositories/"+n, nil)
		}
	})
	e.settings = filepath.Join(e.work, "settings.xml")
	e.write(e.settings, strings.ReplaceAll(mavenE2ESettings, "{{GROUPURL}}", e.repoURL(e.group)))
	gpath := strings.ReplaceAll(e.groupID, ".", "/") + "/greeter/"

	lib := filepath.Join(e.work, "lib")
	t.Run("mvn deploy release", func(t *testing.T) {
		e.libProject(lib, "1.0", "hi")
		e.mustMvn(t, lib, "-B", "deploy")
		m := e.metadata(e.releases, gpath+"maven-metadata.xml")
		if m.Versioning.Release != "1.0" || !mavenE2EContains(m.Versioning.Versions, "1.0") {
			t.Fatalf("release metadata %+v", m.Versioning)
		}
		e.verifyChecksum(e.releases, gpath+"1.0/greeter-1.0.jar")
		e.verifyChecksum(e.releases, gpath+"1.0/greeter-1.0.pom")
	})
	t.Run("mvn deploy snapshot twice", func(t *testing.T) {
		e.libProject(lib, "1.1-SNAPSHOT", "snap")
		e.mustMvn(t, lib, "-B", "deploy")
		e.mustMvn(t, lib, "-B", "deploy")
		m := e.metadata(e.snaps, gpath+"1.1-SNAPSHOT/maven-metadata.xml")
		if m.Versioning.Snapshot.BuildNumber != 2 || len(m.Versioning.SnapshotVersions) == 0 {
			t.Fatalf("snapshot metadata %+v", m.Versioning)
		}
		v := m.Versioning.SnapshotVersions[0].Value
		e.verifyChecksum(e.snaps, gpath+"1.1-SNAPSHOT/greeter-"+v+".jar")
	})
	t.Run("release redeploy rejected", func(t *testing.T) {
		e.libProject(lib, "1.0", "changed")
		if out, err := e.runMvn(t, lib, "-B", "deploy"); err == nil {
			t.Fatalf("redeploy of 1.0 succeeded:\n%s", out)
		} else if !strings.Contains(out, "409") {
			t.Fatalf("redeploy failed without 409:\n%s", out)
		}
	})
	t.Run("mvn install via group", func(t *testing.T) {
		app := filepath.Join(e.work, "app")
		e.write(filepath.Join(app, "pom.xml"), strings.ReplaceAll(mavenE2EAppPOM, "{{GROUP}}", e.groupID))
		e.write(filepath.Join(app, "src/main/java/app/Main.java"), "package app;\npublic class Main { public static void main(String[] a) {"+
			" System.out.println(org.apache.commons.lang3.StringUtils.capitalize(com.acme.Greeter.hi())); } }\n")
		e.mustMvn(t, app, "-B", "install")
		// commons-lang3 came through the proxy and is now cached there.
		e.verifyChecksum(e.proxy, "org/apache/commons/commons-lang3/3.17.0/commons-lang3-3.17.0.jar")
	})
	t.Run("mvn dependency:get via group", func(t *testing.T) {
		e.mustMvn(t, e.work, "-B", "dependency:get", "-Dartifact="+e.groupID+":greeter:1.1-SNAPSHOT", "-Dtransitive=false")
		e.mustMvn(t, e.work, "-B", "dependency:get", "-Dartifact=junit:junit:4.13.2")
	})
	t.Run("group metadata merged", func(t *testing.T) {
		if e.mvn == "" {
			t.Skip("mvn not installed (the versions are deployed with mvn)")
		}
		m := e.metadata(e.group, gpath+"maven-metadata.xml")
		if !mavenE2EContains(m.Versioning.Versions, "1.0") || !mavenE2EContains(m.Versioning.Versions, "1.1-SNAPSHOT") {
			t.Fatalf("group metadata %+v", m.Versioning)
		}
	})
	t.Run("gradle publish and build", func(t *testing.T) {
		if e.gradle == "" {
			t.Skip("gradle not installed")
		}
		e.gradleRun(t)
	})
}

func (e *mavenE2E) api(method, path string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, e.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *mavenE2E) fetch(repo, p string) []byte {
	e.t.Helper()
	code, body := e.api(http.MethodGet, "/repository/"+repo+"/"+p, nil)
	if code != http.StatusOK {
		e.t.Fatalf("GET %s/%s: %d %s", repo, p, code, body)
	}
	return body
}

type mavenE2EMetadata struct {
	Versioning struct {
		Release  string   `xml:"release"`
		Versions []string `xml:"versions>version"`
		Snapshot struct {
			BuildNumber int `xml:"buildNumber"`
		} `xml:"snapshot"`
		SnapshotVersions []struct {
			Extension string `xml:"extension"`
			Value     string `xml:"value"`
		} `xml:"snapshotVersions>snapshotVersion"`
	} `xml:"versioning"`
}

func (e *mavenE2E) metadata(repo, p string) mavenE2EMetadata {
	e.t.Helper()
	var m mavenE2EMetadata
	if err := xml.Unmarshal(e.fetch(repo, p), &m); err != nil {
		e.t.Fatalf("parse %s/%s: %v", repo, p, err)
	}
	return m
}

// verifyChecksum checks that the served .sha1 matches the served file.
func (e *mavenE2E) verifyChecksum(repo, p string) {
	e.t.Helper()
	body := e.fetch(repo, p)
	want := strings.TrimSpace(string(e.fetch(repo, p+".sha1")))
	if got := mavenE2ESHA1(body); got != want {
		e.t.Fatalf("%s/%s: sha1 %s, .sha1 says %s", repo, p, got, want)
	}
}

func (e *mavenE2E) write(p, body string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *mavenE2E) libProject(dir, version, greeting string) {
	r := strings.NewReplacer("{{GROUP}}", e.groupID, "{{VERSION}}", version,
		"{{RELEASES}}", e.repoURL(e.releases), "{{SNAPSHOTS}}", e.repoURL(e.snaps))
	e.write(filepath.Join(dir, "pom.xml"), r.Replace(mavenE2ELibPOM))
	e.write(filepath.Join(dir, "src/main/java/com/acme/Greeter.java"),
		"package com.acme;\npublic class Greeter { public static String hi() { return \""+greeting+"\"; } }\n")
}

// runMvn runs mvn with the e2e settings and a fresh local repository, so
// every dependency is resolved from Hangar.
func (e *mavenE2E) runMvn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	if e.mvn == "" {
		t.Skip("mvn not installed")
	}
	e.localRepoCounter++
	local := filepath.Join(e.work, "m2", strings.Repeat("r", e.localRepoCounter))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.mvn, append([]string{"-s", e.settings, "-Dmaven.repo.local=" + local, "--no-transfer-progress"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HANGAR_E2E_TOKEN="+e.token)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *mavenE2E) mustMvn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := e.runMvn(t, dir, args...); err != nil {
		t.Fatalf("mvn %v: %v\n%s", args, err, out)
	}
}

func (e *mavenE2E) gradleRun(t *testing.T) {
	dir := filepath.Join(e.work, "gradle-lib")
	e.write(filepath.Join(dir, "settings.gradle"), "rootProject.name = 'gradlelib'\n")
	e.write(filepath.Join(dir, "build.gradle"), `plugins { id 'java-library'; id 'maven-publish' }
group = '`+e.groupID+`'
version = '2.0'
publishing {
  publications { maven(MavenPublication) { from components.java } }
  repositories { maven {
    url = '`+e.repoURL(e.releases)+`'
    allowInsecureProtocol = true
    credentials { username = 'e2e'; password = System.getenv('HANGAR_E2E_TOKEN') }
  } }
}
`)
	e.write(filepath.Join(dir, "src/main/java/com/acme/G.java"), "package com.acme; public class G {}\n")
	app := filepath.Join(e.work, "gradle-app")
	e.write(filepath.Join(app, "settings.gradle"), "rootProject.name = 'gradleapp'\n")
	e.write(filepath.Join(app, "build.gradle"), `plugins { id 'java' }
repositories { maven {
  url = '`+e.repoURL(e.group)+`'
  allowInsecureProtocol = true
  credentials { username = 'e2e'; password = System.getenv('HANGAR_E2E_TOKEN') }
} }
dependencies {
  implementation '`+e.groupID+`:gradlelib:2.0'
  implementation 'com.google.guava:guava:33.4.0-jre'
}
`)
	e.write(filepath.Join(app, "src/main/java/app/Main.java"),
		"package app; public class Main { public static void main(String[] a) { System.out.println(com.google.common.base.Strings.repeat(\"x\", 2) + new com.acme.G()); } }\n")
	home := filepath.Join(e.work, "gradle-home")
	for _, step := range []struct{ dir, task string }{{dir, "publish"}, {app, "build"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		cmd := exec.CommandContext(ctx, e.gradle, "--no-daemon", "--console=plain", step.task)
		cmd.Dir = step.dir
		cmd.Env = append(os.Environ(), "GRADLE_USER_HOME="+home, "HANGAR_E2E_TOKEN="+e.token)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("gradle %s: %v\n%s", step.task, err, out)
		}
	}
	gp := strings.ReplaceAll(e.groupID, ".", "/") + "/gradlelib/"
	if m := e.metadata(e.releases, gp+"maven-metadata.xml"); m.Versioning.Release != "2.0" {
		t.Fatalf("gradle publish metadata %+v", m.Versioning)
	}
	e.verifyChecksum(e.releases, gp+"2.0/gradlelib-2.0.module")
	e.verifyChecksum(e.proxy, "com/google/guava/guava/33.4.0-jre/guava-33.4.0-jre.jar")
}

func mavenE2EContains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

func mavenE2ESHA1(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}
