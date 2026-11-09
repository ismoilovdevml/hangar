package maven

import (
	"slices"
	"testing"
)

func TestRoutingIdentity(t *testing.T) {
	for _, c := range []struct{ path, primary, alt string }{
		{"com/edcom/utils/1.0/utils-1.0.jar", "com.edcom:utils", ""},
		{"com/edcom/utils/1.0/utils-1.0.jar.sha1", "com.edcom:utils", ""},
		{"com/edcom/utils/maven-metadata.xml", "com.edcom:utils", "com.edcom.utils"},
		{"com/edcom/utils/1.0-SNAPSHOT/maven-metadata.xml", "com.edcom:utils", ""},
		{"com/maven-metadata.xml", "com", ""},
		{"maven-metadata.xml", "", ""},
		{"archetype-catalog.xml", "", ""},
		{"com/edcom/utils/1.0/notes.txt", "com.edcom:utils", "com.edcom.utils.1.0"},
		{"com/edcom/readme.txt", "", "com.edcom"},
	} {
		mp, err := parsePath(c.path)
		if err != nil {
			t.Fatal(err)
		}
		p, a := routingIdentity(mp)
		if p != c.primary || a != c.alt {
			t.Errorf("%s: identity (%q, %q), want (%q, %q)", c.path, p, a, c.primary, c.alt)
		}
	}
}

func TestListingNames(t *testing.T) {
	got := listingNames("com/edcom/utils/")
	want := []string{"com", "com:edcom", "com.edcom", "com.edcom:utils", "com.edcom.utils"}
	if !slices.Equal(got, want) {
		t.Fatalf("listingNames = %v", got)
	}
	if listingNames("") != nil {
		t.Fatal("root has no names")
	}
}
