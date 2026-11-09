package signing_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
	"github.com/ismoilovdevml/hangar/internal/signing"
)

func keyString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func TestPGStore(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	storeID := pgtest.AddBlobStore(t, pool, "default", "fs", map[string]any{})
	var repoID string
	if err := pool.QueryRow(ctx, `INSERT INTO repositories (name, format, type, blob_store_id) VALUES ('apt-pg', 'apt', 'hosted', $1) RETURNING id::text`,
		storeID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	keyA, keyB := keyString(), keyString()
	kr, _ := secrets.NewKeyring(keyA, nil)
	svc := signing.New(signing.Options{Store: signing.NewPGStore(pool), Secrets: kr, Algorithm: signing.AlgorithmEd25519})
	repo := &core.Repository{ID: repoID, Name: "apt-pg", Format: "apt", Type: core.RepoHosted}
	k, err := svc.KeyFor(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	shared := &core.Repository{ID: repoID, Name: "apt-pg", Format: "apt", Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"signing_key": "shared"}}}
	ks, err := svc.KeyFor(ctx, shared)
	if err != nil || ks.Fingerprint() == k.Fingerprint() {
		t.Fatalf("shared key: %v", err)
	}
	st := signing.NewPGStore(pool)
	if err := st.Create(ctx, &signing.Record{Name: signing.KeyName(repo), RepoID: repoID, Algorithm: "x", Fingerprint: "x", PublicKey: "x", PrivateKey: "x"}); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("duplicate create: %v", err)
	}
	info, err := svc.Rotate(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}

	// Rotating security.secret_key re-wraps the stored signing keys.
	krB, _ := secrets.NewKeyring(keyB, []string{keyA})
	rep, err := secrets.Sweep(ctx, pool, krB, secrets.SweepOptions{Rewrap: true})
	if err != nil || rep.Rewrapped < 2 {
		t.Fatalf("sweep: %+v %v", rep, err)
	}
	onlyB, _ := secrets.NewKeyring(keyB, nil)
	other := signing.New(signing.Options{Store: st, Secrets: onlyB})
	if got, err := other.KeyFor(ctx, repo); err != nil || got.Fingerprint() != info.Fingerprint {
		t.Fatalf("after re-wrap: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, signing.KeyName(repo)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("key outlived its repository: %v", err)
	}
	if _, err := st.Get(ctx, signing.SharedKeyName); err != nil {
		t.Fatalf("shared key: %v", err)
	}
}
