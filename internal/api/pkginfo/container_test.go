package pkginfo

import (
	"context"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestContainerImageSizesFromManifests(t *testing.T) {
	e := newEnv(t)
	r := repo("docker-hosted", "docker", core.RepoHosted)
	ctx := context.Background()

	amd := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:c1","size":100},
		"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:l1","size":3000},
			{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:l2","size":500}]}`
	arm := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:c2","size":90},
		"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:l3","size":2000}]}`
	idx := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:arm","size":400,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}},
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:amd","size":400,"platform":{"os":"linux","architecture":"amd64"}},
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:att","size":400,"platform":{"os":"unknown","architecture":"unknown"}},
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:s390","size":400,"platform":{"os":"linux","architecture":"s390x"}}]}`
	e.publish(r, &core.Component{Namespace: "team", Name: "app", Version: "1.0"},
		file{"team/app/manifests/sha256:amd", []byte(amd)}, file{"team/app/manifests/sha256:arm", []byte(arm)},
		file{"team/app/manifests/sha256:idx", []byte(idx)})

	img, err := e.svc.ContainerImage(ctx, r, "team", "app", "sha256:amd", "application/vnd.oci.image.manifest.v1+json")
	if err != nil || img == nil {
		t.Fatalf("image = %v, %v", img, err)
	}
	if *img.SizeBytes != 3600 || len(img.Layers) != 2 || img.Layers[0].SizeBytes != 3000 || img.Config.SizeBytes != 100 || img.Platform != "" {
		t.Errorf("single-platform image = %+v", img)
	}

	// An index is sized by linux/amd64; attestations are not platforms; an
	// unstored platform has no size.
	img, err = e.svc.ContainerImage(ctx, r, "team", "app", "sha256:idx", "application/vnd.oci.image.index.v1+json")
	if err != nil || img == nil {
		t.Fatalf("index = %v, %v", img, err)
	}
	if img.Platform != "linux/amd64" || *img.SizeBytes != 3600 || len(img.Layers) != 2 || len(img.Platforms) != 3 {
		t.Fatalf("index = %+v", img)
	}
	if p := img.Platforms[0]; p.Platform != "linux/arm64/v8" || p.SizeBytes == nil || *p.SizeBytes != 2090 {
		t.Errorf("arm64 = %+v", p)
	}
	if p := img.Platforms[2]; p.Platform != "linux/s390x" || p.SizeBytes != nil {
		t.Errorf("unstored platform = %+v", p)
	}

	if img, err := e.svc.ContainerImage(ctx, r, "team", "app", "sha256:missing", ""); img != nil || err != nil {
		t.Errorf("missing manifest = %v, %v", img, err)
	}
	if img, err := e.svc.ContainerImage(ctx, repo("npm-hosted", "npm", core.RepoHosted), "", "x", "sha256:amd", ""); img != nil || err != nil {
		t.Errorf("not docker = %v, %v", img, err)
	}
}
