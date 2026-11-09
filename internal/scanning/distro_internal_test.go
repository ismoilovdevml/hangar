package scanning

import "testing"

func TestInferDistro(t *testing.T) {
	cases := []struct {
		format, remote string
		suites         []string
		want           string
	}{
		{"apt", "http://deb.debian.org/debian", []string{"bookworm", "bookworm-updates"}, "Debian:12"},
		{"apt", "http://security.debian.org/debian-security", []string{"bookworm-security"}, "Debian:12"},
		{"apt", "https://mirror.example.org/debian/", []string{"trixie", "stable"}, "Debian:13"},
		{"apt", "http://archive.ubuntu.com/ubuntu", []string{"jammy", "jammy-updates", "jammy-security"}, "Ubuntu:22.04:LTS"},
		{"apt", "http://ports.ubuntu.com/ubuntu-ports", []string{"noble"}, "Ubuntu:24.04:LTS"},
		{"apt", "http://deb.debian.org/debian", []string{"bookworm", "trixie"}, ""},
		{"apt", "http://deb.debian.org/debian", []string{"sid"}, ""},
		{"apt", "http://deb.debian.org/debian", nil, ""},
		{"apt", "https://apt.example.com/repo", []string{"bookworm"}, ""},
		{"rpm", "https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/", nil, "Rocky Linux:9"},
		{"rpm", "https://dl.rockylinux.org/vault/rocky/8.9/AppStream/x86_64/os", nil, "Rocky Linux:8"},
		{"rpm", "https://repo.almalinux.org/almalinux/9/AppStream/x86_64/os/", nil, "AlmaLinux:9"},
		{"rpm", "https://mirror.example.org/almalinux/10/BaseOS/x86_64/os/", nil, "AlmaLinux:10"},
		{"rpm", "https://cdn.redhat.com/content/dist/rhel9/9/x86_64/baseos/os", nil, "Red Hat:enterprise_linux:9::baseos"},
		{"rpm", "https://dl.rockylinux.org/pub/rocky/", nil, ""},
		{"rpm", "https://rpm.example.com/el9/x86_64/", nil, ""},
		{"rpm", "::not a url", nil, ""},
	}
	for _, c := range cases {
		if got := InferDistro(c.format, c.remote, c.suites); got != c.want {
			t.Errorf("InferDistro(%s, %s, %v) = %q, want %q", c.format, c.remote, c.suites, got, c.want)
		}
	}
}

func TestDistroPackage(t *testing.T) {
	for file, want := range map[string]string{
		"openssl-3.0.7-27.el9.src.rpm":       "openssl",
		"python3.11-3.11.7-1.el9.src.rpm":    "python3.11",
		"kernel-rt-5.14.0-427.el9.nosrc.rpm": "kernel-rt",
		"openssl-3.0.7-27.el9.x86_64.rpm":    "",
		"broken.src.rpm":                     "",
	} {
		if got := sourceRPMName(file); got != want {
			t.Errorf("sourceRPMName(%q) = %q, want %q", file, got, want)
		}
	}
	if n := PackageName("rpm", "", "openssl-libs", map[string]any{"sourcerpm": "openssl-3.0.7-27.el9.src.rpm"}); n != "openssl" {
		t.Errorf("rpm name = %q", n)
	}
	if n := PackageName("rpm", "", "custom", map[string]any{}); n != "custom" {
		t.Errorf("rpm name without sourcerpm = %q", n)
	}
	if n := PackageName("apt", "", "libssl3", map[string]any{"source": "openssl"}); n != "openssl" {
		t.Errorf("apt name = %q", n)
	}
	if v := PackageVersion("apt", "1.2-3+b1", map[string]any{"source_version": "1.2-3"}); v != "1.2-3" {
		t.Errorf("apt version = %q", v)
	}
	if v := PackageVersion("rpm", "1:3.0.7-27.el9", nil); v != "1:3.0.7-27.el9" {
		t.Errorf("rpm version = %q", v)
	}
	if c := UpgradeCommand("apt", "openssl", "3.0.11-1~deb12u2"); c != "apt-get install --only-upgrade openssl" {
		t.Errorf("apt command = %q", c)
	}
	if c := UpgradeCommand("rpm", "openssl", "1:3.0.7-28.el9_4"); c != "dnf upgrade openssl" {
		t.Errorf("rpm command = %q", c)
	}
}
