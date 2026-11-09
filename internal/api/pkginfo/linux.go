package pkginfo

import "context"

// ---------------------------------------------------------------- APT, RPM

// linuxPackage reads what the apt and rpm formats keep on the component
// from the package's control file or RPM header: the summary, the long
// description (shown as the README, plain text), the homepage and, for
// RPM, the licence. Debian binary packages declare no licence field.
func (x *extraction) linuxPackage(_ context.Context) {
	str := func(k string) string { s, _ := x.c.Attributes[k].(string); return s }
	x.setDescription(str("summary"))
	x.setHomepage(str("homepage"))
	x.addLicense(str("license"))
	source := "the control file"
	if x.repo.Format == "rpm" {
		source = "the RPM header"
	}
	x.setReadme(str("description"), FormatText, "the description in "+source)
}
