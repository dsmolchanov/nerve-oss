// Package hybrid connects a self-hosted Nerve runtime to a Cloud mailbox.
//
// The runtime holds mail locally and reaches Cloud only for transport: it pulls
// inbound deliveries and hands outbound messages to the Cloud provider. It
// authenticates with OAuth client credentials using private_key_jwt, so the
// only long-lived secret on this host is an RSA private key that never leaves
// it. Access tokens are short-lived and re-minted from that key; there is no
// refresh token to steal or to keep in sync.
package hybrid

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"neuralmail/internal/auth"
)

// StateVersion is the on-disk format. A file written by a newer runtime is
// refused rather than reinterpreted: a misread installation would re-pair or
// sign with the wrong key, and both are worse than refusing to start.
const StateVersion = 1

const (
	// A 2048-bit modulus is the floor auth.RSAJWKThumbprint accepts. Generate
	// larger: this key outlives many access tokens and rotating it costs an
	// operator round trip through the protected M2M inventory.
	installationKeyBits = 3072

	maxStateBytes = 64 << 10
	stateFileMode = fs.FileMode(0o600)
	stateDirMode  = fs.FileMode(0o700)
)

var (
	// ErrNotConnected means no installation state exists. It is the ordinary
	// state of a runtime that has never been paired, never an error to log.
	ErrNotConnected = errors.New("hybrid installation not connected")

	ErrStateUnsafePermissions = errors.New("hybrid installation state is readable beyond its owner")
	ErrStateVersion           = errors.New("hybrid installation state was written by a newer runtime")
	ErrNoPendingKey           = errors.New("hybrid installation has no prepared replacement key")
)

// Key is one RSA keypair admitted to the Cloud M2M inventory. KID is the
// RFC 7638 thumbprint of the public key, which is what Cloud registers and what
// every assertion header must carry.
type Key struct {
	KID           string `json:"kid"`
	PrivateKeyPEM string `json:"private_key_pem"`
}

// State is the whole durable identity of one hybrid installation. It is written
// as a single file so that rotation, which replaces the key and the key ID
// together, cannot be observed half-applied.
//
// PendingKey is a replacement generated locally but not yet in use. It becomes
// Key only once Cloud has minted a token for it, so a replacement that was
// never admitted cannot strand the runtime without a usable identity.
type State struct {
	Version        int    `json:"version"`
	CloudBaseURL   string `json:"cloud_base_url"`
	TokenEndpoint  string `json:"token_endpoint"`
	Resource       string `json:"resource"`
	ClientID       string `json:"client_id"`
	Generation     int64  `json:"generation"`
	OrgID          string `json:"org_id"`
	InstallationID string `json:"installation_id"`
	InboxID        string `json:"inbox_id"`
	AuthorityID    string `json:"authority_id"`
	Key            Key    `json:"key"`
	PendingKey     *Key   `json:"pending_key,omitempty"`
}

// Redacted returns the state with every private key removed, for status output
// and logs. Nothing that prints a State may print the original.
func (s State) Redacted() State {
	s.Key.PrivateKeyPEM = ""
	if s.PendingKey != nil {
		pending := *s.PendingKey
		pending.PrivateKeyPEM = ""
		s.PendingKey = &pending
	}
	return s
}

func (s State) Validate() error {
	if s.Version != StateVersion {
		if s.Version > StateVersion {
			return ErrStateVersion
		}
		return fmt.Errorf("unsupported hybrid state version %d", s.Version)
	}
	if err := validateEndpoint("cloud_base_url", s.CloudBaseURL); err != nil {
		return err
	}
	if err := validateEndpoint("token_endpoint", s.TokenEndpoint); err != nil {
		return err
	}
	if strings.TrimSpace(s.Resource) != s.Resource || s.Resource == "" {
		return errors.New("hybrid resource must be set")
	}
	if !validClientID(s.ClientID) {
		return errors.New("hybrid client_id must be printable ASCII")
	}
	if s.Generation <= 0 {
		return errors.New("hybrid generation must be positive")
	}
	for name, value := range map[string]string{
		"org_id": s.OrgID, "installation_id": s.InstallationID, "inbox_id": s.InboxID,
	} {
		if parsed, err := uuid.Parse(value); err != nil || parsed.String() != value {
			return fmt.Errorf("hybrid %s must be a canonical UUID", name)
		}
	}
	// The authority ID namespaces local deduplication keys. A colon would let
	// one authority forge another's key by splitting the joined form.
	if s.AuthorityID == "" || strings.ContainsAny(s.AuthorityID, ":") || strings.TrimSpace(s.AuthorityID) != s.AuthorityID {
		return errors.New("hybrid authority_id must be non-empty and contain no colon")
	}
	if err := validateKey("key", s.Key); err != nil {
		return err
	}
	if s.PendingKey != nil {
		if err := validateKey("pending_key", *s.PendingKey); err != nil {
			return err
		}
		if s.PendingKey.KID == s.Key.KID {
			return errors.New("hybrid pending_key must differ from the active key")
		}
	}
	return nil
}

func validateKey(name string, key Key) error {
	parsed, err := ParsePrivateKeyPEM(key.PrivateKeyPEM)
	if err != nil {
		return fmt.Errorf("hybrid %s: %w", name, err)
	}
	thumbprint, err := auth.RSAJWKThumbprint(&parsed.PublicKey)
	if err != nil {
		return fmt.Errorf("hybrid %s: %w", name, err)
	}
	// Cloud looks the key up by the kid in the assertion header and then
	// verifies with the JWK it has stored. A stored kid that is not this key's
	// thumbprint would send every assertion to the wrong registered key.
	if key.KID != thumbprint {
		return fmt.Errorf("hybrid %s kid is not the RFC 7638 thumbprint of its private key", name)
	}
	return nil
}

// validateEndpoint accepts HTTPS anywhere and plaintext HTTP only on loopback,
// where there is no network to intercept. A bearer token or a signed assertion
// must never cross a plaintext hop.
func validateEndpoint(name, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("hybrid %s must be an absolute URL", name)
	}
	switch {
	case parsed.Scheme == "https":
	case parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()):
	default:
		return fmt.Errorf("hybrid %s must use https, or http on loopback", name)
	}
	if parsed.User != nil {
		return fmt.Errorf("hybrid %s must not embed credentials", name)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("hybrid %s must not carry a query or fragment", name)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func validClientID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

// GenerateKey produces a new installation keypair and its Cloud key ID.
func GenerateKey() (Key, error) {
	private, err := rsa.GenerateKey(rand.Reader, installationKeyBits)
	if err != nil {
		return Key{}, err
	}
	thumbprint, err := auth.RSAJWKThumbprint(&private.PublicKey)
	if err != nil {
		return Key{}, err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return Key{}, err
	}
	return Key{
		KID:           thumbprint,
		PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})),
	}, nil
}

func ParsePrivateKeyPEM(raw string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("expected one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	private, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("hybrid installation key must be RSA")
	}
	if err := private.Validate(); err != nil {
		return nil, err
	}
	if private.N.BitLen() < 2048 {
		return nil, errors.New("hybrid installation key must be at least 2048 bits")
	}
	return private, nil
}

// publicJWK is exactly the shape the Cloud M2M inventory admits: every member
// required, no extras, key_ops pinned to verify so registry data can never
// smuggle a signing capability.
type publicJWK struct {
	KTY    string   `json:"kty"`
	KID    string   `json:"kid"`
	Use    string   `json:"use"`
	Alg    string   `json:"alg"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	KeyOps []string `json:"key_ops"`
}

// PublicJWK renders the key the way the Cloud M2M inventory admits it. The
// operator pastes this during admission; it carries no private material.
func (k Key) PublicJWK() (json.RawMessage, error) {
	private, err := ParsePrivateKeyPEM(k.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	return json.Marshal(publicJWK{
		KTY: "RSA", KID: k.KID, Use: "sig", Alg: assertionAlgorithm,
		N:      base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()),
		E:      base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.PublicKey.E)).Bytes()),
		KeyOps: []string{"verify"},
	})
}

// Store reads and writes installation state at one path.
type Store struct{ Path string }

// Load returns the installation state, or ErrNotConnected when this runtime has
// never been paired. It refuses state that any account but the owner can read:
// the file holds the private key, so loose permissions are a disclosed key, and
// continuing would be worse than failing to start.
func (s Store) Load() (State, error) {
	file, err := os.Open(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, ErrNotConnected
	}
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return State{}, err
	}
	if !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("hybrid installation state %s is not a regular file", s.Path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return State{}, fmt.Errorf("%w: %s has mode %04o", ErrStateUnsafePermissions, s.Path, info.Mode().Perm())
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return State{}, err
	}
	if len(raw) > maxStateBytes {
		return State{}, errors.New("hybrid installation state is too large")
	}
	var state State
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		// A file from a newer runtime most often fails here, on a field this
		// version does not know. Report that rather than a JSON error.
		var probe struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Version > StateVersion {
			return State{}, ErrStateVersion
		}
		return State{}, fmt.Errorf("hybrid installation state is unreadable: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return State{}, errors.New("hybrid installation state has trailing content")
	}
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

// Save replaces the state atomically. A crash leaves either the previous
// installation or the new one, never a truncated key, so an interrupted
// rotation is always recoverable from one side or the other.
func (s Store) Save(state State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	directory := filepath.Dir(s.Path)
	if err := os.MkdirAll(directory, stateDirMode); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Create the temporary file in the destination directory so the rename is
	// within one filesystem and therefore atomic, and at the final mode so the
	// key is never briefly world-readable.
	temporary, err := os.CreateTemp(directory, ".hybrid-installation-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() {
		temporary.Close()
		os.Remove(name)
	}()
	if err := temporary.Chmod(stateFileMode); err != nil {
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.Path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

// Remove deletes the installation state. A runtime that has been disconnected
// keeps no private key: the key is useless once Cloud has revoked it, and a
// retained one is only a way to leak an identity later.
func (s Store) Remove() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(s.Path))
}

// syncDirectory makes the rename or unlink durable. Without it a power loss can
// leave the directory entry pointing at the old file even though the data was
// written.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, fs.ErrInvalid) {
		return err
	}
	return nil
}
