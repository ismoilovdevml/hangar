package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/scanning"
	"github.com/ismoilovdevml/hangar/internal/scanning/trivy"
)

// imageScanning builds container image scanning from scanner.*: the Trivy
// client (through the scan adapter) and the registry glue that lets it
// pull images from this server by digest. It returns zero options when
// scanner.trivy_url is not set.
func imageScanning(cfg config.Config, df *docker.Format) (scanning.Options, error) {
	sc := cfg.Scanner
	if !sc.ImageScanning() {
		return scanning.Options{}, nil
	}
	if df == nil {
		return scanning.Options{}, errors.New("scanner.trivy_url: the docker format is not available")
	}
	client, err := trivy.New(trivy.Options{ServerURL: sc.TrivyURL, ServerToken: sc.TrivyToken,
		AdapterURL: sc.AdapterURL, AdapterToken: sc.AdapterToken, Timeout: sc.Timeout})
	if err != nil {
		return scanning.Options{}, err
	}
	raw, name := sc.RegistryURL, "scanner.registry_url"
	if raw == "" {
		raw, name = cfg.Server.BaseURL, "server.base_url"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return scanning.Options{}, fmt.Errorf("%s %q: the image scanner needs the registry URL (http:// or https://)", name, raw)
	}
	reg := &imageRegistry{format: df, host: u.Host}
	return scanning.Options{Images: client, Registry: reg, RegistryHost: u.Host, RegistryInsecure: u.Scheme == "http",
		ImagePlatforms: sc.Platforms, ImageWorkers: sc.Workers}, nil
}

// imageRegistry adapts the docker format to scanning.ImageRegistry.
type imageRegistry struct {
	format *docker.Format
	host   string
}

func (r *imageRegistry) Manifests(ctx context.Context, repository, image, digest string) ([]scanning.ImageManifest, error) {
	list, err := r.format.ScanManifests(ctx, repository, image, digest)
	if errors.Is(err, docker.ErrNotImage) {
		return nil, fmt.Errorf("%w: %w", scanning.ErrNotImage, err)
	}
	if err != nil {
		return nil, err
	}
	out := make([]scanning.ImageManifest, len(list))
	for i, m := range list {
		out[i] = scanning.ImageManifest{Digest: m.Digest, Platform: m.Platform}
	}
	return out, nil
}

func (r *imageRegistry) PullToken(repository, image string) (string, error) {
	return r.format.ScanToken(repository, image, r.host)
}
