package maven_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestRealGradle publishes with the real `gradle` client (maven-publish) into
// hosted repositories, then resolves the release, the SNAPSHOT and a
// proxied dependency through a group, all against an in-process server.
// Skipped when gradle is not installed or with -short.
func TestRealGradle(t *testing.T) {
	gradle, err := exec.LookPath("gradle")
	if err != nil {
		t.Skip("gradle not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped with -short")
	}
	f := newFixture(t)
	rm := newRemote()
	rm.set("org/example/remote/3.1/remote-3.1.pom", pom("org.example", "remote", "3.1", "jar"))
	rm.set("org/example/remote/3.1/remote-3.1.jar", emptyJar)
	rm.set("org/example/remote/maven-metadata.xml", clientMetadata("org.example", "remote", "3.1"))
	f.addProxy("central", rm, core.ProxyConfig{}, map[string]any{"version_policy": "release"})
	f.addGroup("public", "releases", "snapshots", "central")

	home := t.TempDir() // isolated GRADLE_USER_HOME
	run := func(dir string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, gradle, append([]string{"--no-daemon", "--console=plain", "--stacktrace"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GRADLE_USER_HOME="+home, "HANGAR_USER="+user, "HANGAR_PASSWORD="+pass)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gradle %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	lib := writeGradleLib(t, f.srv.URL)
	run(lib, "publish", "-Pver=1.0")
	run(lib, "publish", "-Pver=1.1-SNAPSHOT")
	run(lib, "publish", "-Pver=1.1-SNAPSHOT") // second snapshot build

	auth := "reader:r"
	m := parse(t, f.getVerified("releases/com/acme/greeter/maven-metadata.xml", auth))
	if !slices.Equal(m.Versioning.Versions, []string{"1.0"}) || m.Versioning.Release != "1.0" {
		t.Fatalf("release metadata %+v", m.Versioning)
	}
	for _, ext := range []string{"jar", "pom", "module"} {
		f.getVerified("releases/com/acme/greeter/1.0/greeter-1.0."+ext, auth)
	}
	sm := parse(t, f.getVerified("snapshots/com/acme/greeter/1.1-SNAPSHOT/maven-metadata.xml", auth))
	if sm.Versioning.Snapshot == nil || sm.Versioning.Snapshot.BuildNumber != 2 {
		t.Fatalf("snapshot metadata %+v", sm.Versioning)
	}
	// Redeploying a release into the allow_once repository fails the build.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := os.WriteFile(filepath.Join(lib, "src/main/java/com/acme/Greeter.java"),
		[]byte("package com.acme; public class Greeter { public static String hi() { return \"changed\"; } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, gradle, "--no-daemon", "--console=plain", "publish", "-Pver=1.0")
	cmd.Dir = lib
	cmd.Env = append(os.Environ(), "GRADLE_USER_HOME="+home, "HANGAR_USER="+user, "HANGAR_PASSWORD="+pass)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("release redeploy succeeded:\n%s", out)
	} else if !strings.Contains(string(out), "409") {
		t.Errorf("release redeploy failed without a 409:\n%s", out)
	}

	app := writeGradleApp(t, f.srv.URL)
	out := run(app, "printDeps")
	t.Logf("gradle resolved:\n%s", grepLines(out, "DEP "))
	for _, want := range []string{"greeter-1.0.jar", "greeter-1.1-", "remote-3.1.jar"} {
		if !strings.Contains(out, want) {
			t.Errorf("resolved classpath lacks %s:\n%s", want, out)
		}
	}
	if rm.count("org/example/remote/3.1/remote-3.1.jar") != 1 {
		t.Errorf("proxied jar fetched %d times", rm.count("org/example/remote/3.1/remote-3.1.jar"))
	}
	run(app, "build")
}

// emptyJar is a valid empty zip archive.
const emptyJar = "PK\x05\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeGradleLib(t *testing.T, base string) string {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"settings.gradle": "rootProject.name = 'greeter'\n",
		"build.gradle": `plugins { id 'java-library'; id 'maven-publish' }
group = 'com.acme'
version = project.property('ver')
java { withSourcesJar() }
publishing {
  publications { maven(MavenPublication) { from components.java } }
  repositories {
    maven {
      url = version.endsWith('SNAPSHOT') ? '` + base + `/repository/snapshots/' : '` + base + `/repository/releases/'
      allowInsecureProtocol = true
      credentials { username = System.getenv('HANGAR_USER'); password = System.getenv('HANGAR_PASSWORD') }
    }
  }
}
`,
		"src/main/java/com/acme/Greeter.java": "package com.acme; public class Greeter { public static String hi() { return \"hi\"; } }\n",
	})
	return dir
}

func writeGradleApp(t *testing.T, base string) string {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"settings.gradle": "rootProject.name = 'app'\n",
		"build.gradle": `plugins { id 'java' }
repositories {
  maven {
    url = '` + base + `/repository/public/'
    allowInsecureProtocol = true
    credentials { username = 'reader'; password = 'r' }
  }
}
configurations { snap }
dependencies {
  implementation 'com.acme:greeter:1.0'
  implementation 'org.example:remote:3.+'
  snap 'com.acme:greeter:1.1-SNAPSHOT'
  snap 'com.acme:greeter:1.1-SNAPSHOT:sources'
}
tasks.register('printDeps') {
  def cp = configurations.runtimeClasspath
  def sn = configurations.snap
  doLast { (cp.files + sn.files).each { println "DEP " + it.name } }
}
`,
		"src/main/java/app/Main.java": "package app; public class Main { public static void main(String[] a) { System.out.println(com.acme.Greeter.hi()); } }\n",
	})
	return dir
}

func grepLines(s, prefix string) string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
