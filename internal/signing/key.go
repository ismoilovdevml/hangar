// Package signing holds the OpenPGP keys that sign repository metadata:
// APT Release files (InRelease, Release.gpg) and RPM repomd.xml
// (repomd.xml.asc). Each repository gets its own key, or repositories share
// one server-wide key (format setting signing_key: shared). Private keys are
// stored sealed with the secrets keyring (security.secret_key); they never
// leave the server and are never returned by the API.
//
// Keys are RSA 4096 by default, which every apt, gpgv, rpm and dnf release
// in use accepts (Debian 10+, Ubuntu 18.04+, RHEL/Rocky 8 and 9). Ed25519
// keys are smaller and faster but need gpg 2.1+ and rpm with EdDSA support.
package signing

import (
	"bytes"
	"crypto"
	"fmt"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Key algorithms.
const (
	AlgorithmRSA4096 = "rsa4096"
	AlgorithmEd25519 = "ed25519"
	// DefaultAlgorithm is used for generated keys.
	DefaultAlgorithm = AlgorithmRSA4096
)

// ValidAlgorithm reports whether a is a key algorithm keys can be generated with.
func ValidAlgorithm(a string) bool { return a == AlgorithmRSA4096 || a == AlgorithmEd25519 }

// Key is an OpenPGP key that can sign. It is safe for concurrent use.
type Key struct {
	entity    *openpgp.Entity
	algorithm string
}

// baseConfig is the configuration of every generated key and signature:
// SHA-256 everywhere (RHEL 9 refuses SHA-1), and key flags and creation
// times as non-critical subpackets, which old rpm and yum releases (RHEL 7
// and 8 era) require to import a key.
func baseConfig(algorithm string) *packet.Config {
	cfg := &packet.Config{
		DefaultHash:                                      crypto.SHA256,
		InsecureGenerateNonCriticalKeyFlags:              true,
		InsecureGenerateNonCriticalSignatureCreationTime: true,
	}
	switch algorithm {
	case AlgorithmEd25519:
		cfg.Algorithm, cfg.Curve = packet.PubKeyAlgoEdDSA, packet.Curve25519
	default:
		cfg.Algorithm, cfg.RSABits = packet.PubKeyAlgoRSA, 4096
		// RSA PKCS#1 v1.5 signatures need no randomising salt notation;
		// leaving it out keeps them readable by the oldest parsers.
		off := false
		cfg.NonDeterministicSignaturesViaNotation = &off
	}
	return cfg
}

// Generate creates a key for uid (a user id such as
// "Hangar apt-hosted <apt-hosted@hangar.example.com>" or a plain name).
func Generate(algorithm, uid string) (*Key, error) {
	if algorithm == "" {
		algorithm = DefaultAlgorithm
	}
	if !ValidAlgorithm(algorithm) {
		return nil, fmt.Errorf("key algorithm %q: want %s or %s: %w", algorithm, AlgorithmRSA4096, AlgorithmEd25519, core.ErrInvalid)
	}
	name, email := splitUID(uid)
	e, err := openpgp.NewEntity(name, "", email, baseConfig(algorithm))
	if err != nil {
		return nil, fmt.Errorf("generate %s key: %w", algorithm, err)
	}
	return &Key{entity: e, algorithm: algorithm}, nil
}

// splitUID turns "Name <mail>" into its parts; characters NewEntity refuses
// are replaced.
func splitUID(uid string) (name, email string) {
	clean := strings.NewReplacer("(", "", ")", "", "\x00", "")
	name = uid
	if i := strings.LastIndex(uid, "<"); i >= 0 && strings.HasSuffix(uid, ">") {
		name, email = strings.TrimSpace(uid[:i]), uid[i+1:len(uid)-1]
	}
	name = strings.NewReplacer("<", "", ">", "").Replace(clean.Replace(name))
	email = strings.NewReplacer("<", "", ">", "").Replace(clean.Replace(email))
	return name, email
}

// ParsePrivate reads an ASCII-armored (or binary) private key, decrypting
// it with passphrase when it is protected. The first key of the input is
// used; it must be able to sign.
func ParsePrivate(data []byte, passphrase string) (*Key, error) {
	var (
		list openpgp.EntityList
		err  error
	)
	if bytes.Contains(data, []byte("-----BEGIN PGP")) {
		list, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
	} else {
		list, err = openpgp.ReadKeyRing(bytes.NewReader(data))
	}
	if err != nil {
		return nil, fmt.Errorf("read private key: %s: %w", err.Error(), core.ErrInvalid)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no key in the input: %w", core.ErrInvalid)
	}
	e := list[0]
	if e.PrivateKey == nil {
		return nil, fmt.Errorf("the input is a public key; a private key is required: %w", core.ErrInvalid)
	}
	if e.PrivateKey.Encrypted {
		if passphrase == "" {
			return nil, fmt.Errorf("the private key is protected by a passphrase: %w", core.ErrInvalid)
		}
		if err := e.DecryptPrivateKeys([]byte(passphrase)); err != nil {
			return nil, fmt.Errorf("decrypt private key: wrong passphrase: %w", core.ErrInvalid)
		}
	}
	k := &Key{entity: e, algorithm: algorithmOf(e.PrimaryKey)}
	if _, ok := e.SigningKey(time.Now()); !ok {
		return nil, fmt.Errorf("key %s has no valid signing key (expired, revoked or not allowed to sign): %w", k.Fingerprint(), core.ErrInvalid)
	}
	return k, nil
}

func algorithmOf(pk *packet.PublicKey) string {
	switch pk.PubKeyAlgo {
	case packet.PubKeyAlgoRSA, packet.PubKeyAlgoRSASignOnly:
		bits, _ := pk.BitLength()
		return fmt.Sprintf("rsa%d", bits)
	case packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoEd25519:
		return AlgorithmEd25519
	case packet.PubKeyAlgoEd448:
		return "ed448"
	case packet.PubKeyAlgoECDSA:
		return "ecdsa"
	}
	return fmt.Sprintf("algo%d", pk.PubKeyAlgo)
}

// Algorithm names the key type: rsa4096, ed25519, ...
func (k *Key) Algorithm() string { return k.algorithm }

// Fingerprint is the upper-case hex fingerprint of the primary key.
func (k *Key) Fingerprint() string {
	return strings.ToUpper(fmt.Sprintf("%x", k.entity.PrimaryKey.Fingerprint))
}

// KeyID is the long key id (last 16 hex digits of the fingerprint).
func (k *Key) KeyID() string { return strings.ToUpper(k.entity.PrimaryKey.KeyIdString()) }

// CreatedAt is the key's creation time.
func (k *Key) CreatedAt() time.Time { return k.entity.PrimaryKey.CreationTime.UTC() }

// UserID is the key's primary user id.
func (k *Key) UserID() string {
	if id := k.entity.PrimaryIdentity(); id != nil {
		return id.Name
	}
	return ""
}

// PublicBinary is the transferable public key, as `gpg --export` writes it
// (the format of /etc/apt/keyrings/*.gpg).
func (k *Key) PublicBinary() ([]byte, error) {
	var b bytes.Buffer
	if err := k.entity.Serialize(&b); err != nil {
		return nil, fmt.Errorf("serialize public key: %w", err)
	}
	return b.Bytes(), nil
}

// PublicArmored is the ASCII-armored public key (`gpg --armor --export`).
func (k *Key) PublicArmored() ([]byte, error) {
	bin, err := k.PublicBinary()
	if err != nil {
		return nil, err
	}
	return armorBytes(bin, openpgp.PublicKeyType)
}

// PrivateArmored is the ASCII-armored private key, unprotected. It is only
// ever written sealed into the key store.
func (k *Key) PrivateArmored() ([]byte, error) {
	var bin bytes.Buffer
	if err := k.entity.SerializePrivateWithoutSigning(&bin, nil); err != nil {
		return nil, fmt.Errorf("serialize private key: %w", err)
	}
	return armorBytes(bin.Bytes(), openpgp.PrivateKeyType)
}

func armorBytes(b []byte, blockType string) ([]byte, error) {
	var out bytes.Buffer
	w, err := armor.Encode(&out, blockType, nil)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// sigConfig is the configuration of one signature made at t.
func (k *Key) sigConfig(t time.Time) *packet.Config {
	cfg := baseConfig(k.algorithm)
	if !t.IsZero() {
		cfg.Time = func() time.Time { return t }
	}
	return cfg
}

// ClearSign returns msg as an OpenPGP cleartext-signed message (an APT
// InRelease file). at is the signature time; zero means now.
func (k *Key) ClearSign(msg []byte, at time.Time) ([]byte, error) {
	signer, ok := k.entity.SigningKey(time.Now())
	if !ok || signer.PrivateKey == nil {
		return nil, fmt.Errorf("key %s cannot sign", k.Fingerprint())
	}
	var out bytes.Buffer
	w, err := clearsign.Encode(&out, signer.PrivateKey, k.sigConfig(at))
	if err != nil {
		return nil, fmt.Errorf("clearsign: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return nil, fmt.Errorf("clearsign: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("clearsign: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// DetachSign returns an ASCII-armored detached signature of msg (APT
// Release.gpg, RPM repomd.xml.asc). at is the signature time; zero means now.
func (k *Key) DetachSign(msg []byte, at time.Time) ([]byte, error) {
	var out bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&out, k.entity, bytes.NewReader(msg), k.sigConfig(at)); err != nil {
		return nil, fmt.Errorf("detach sign: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// Verify checks an armored detached signature of msg against the key, for
// tests and the admin API's self-check.
func (k *Key) Verify(msg, armoredSig []byte) error {
	_, err := openpgp.CheckArmoredDetachedSignature(openpgp.EntityList{k.entity}, bytes.NewReader(msg), bytes.NewReader(armoredSig), nil)
	return err
}
