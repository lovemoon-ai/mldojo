// Package secrets implements the secret layer: values are
// encrypted with age to the server's master key and stored in PostgreSQL.
// The master key is unlocked from the OS keychain first, then
// MLDOJO_MASTER_KEY, then a key file, or pushed by `mldojo secret unlock`.
package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"filippo.io/age"
	"github.com/lovemoon-ai/mldojo/api/internal/models"
	"github.com/lovemoon-ai/mldojo/internal/keychain"
	v1 "github.com/lovemoon-ai/mldojo/proto/mldojo/v1"
)

var ErrLocked = errors.New("secrets are locked: run `mldojo secret unlock` or set MLDOJO_MASTER_KEY")

const recipientSetting = "secrets.recipient"

type Manager struct {
	store   *models.Store
	keyFile string

	mu     sync.RWMutex
	id     *age.X25519Identity
	source string
}

func New(store *models.Store, keyFile string) *Manager {
	return &Manager{store: store, keyFile: keyFile}
}

var refRe = regexp.MustCompile(`^secret://([a-z0-9_-]+)/([A-Za-z0-9._@-]+)$`)
var partRe = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

// ParseRef splits secret://ns/name.
func ParseRef(ref string) (ns, name string, ok bool) {
	m := refRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// IsRef reports whether s looks like a secret reference.
func IsRef(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "secret://") }

// AutoUnlock tries keychain → env → key file. On a fresh install (no stored
// recipient) it generates a master key and saves it (keychain, else file).
func (m *Manager) AutoUnlock(ctx context.Context) error {
	var stored string
	has, err := m.store.GetSetting(ctx, recipientSetting, &stored)
	if err != nil {
		return err
	}
	try := func(key, source string) bool {
		if strings.TrimSpace(key) == "" {
			return false
		}
		if err := m.unlock(key, source, stored); err != nil {
			slog.Warn("secrets: master key rejected", "source", source, "err", err)
			return false
		}
		return true
	}
	if k, err := keychain.Get(); err == nil && try(k, "keychain") {
		return nil
	}
	if try(os.Getenv("MLDOJO_MASTER_KEY"), "env") {
		return nil
	}
	if b, err := os.ReadFile(m.keyFile); err == nil && try(string(b), "file") {
		return nil
	}
	if has {
		slog.Warn("secrets: locked; unlock with `mldojo secret unlock`")
		return nil
	}
	// First start: generate a master key.
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	source := "keychain"
	if err := keychain.Set(id.String()); err != nil {
		source = "file"
		if err := os.MkdirAll(filepath.Dir(m.keyFile), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(m.keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
			return err
		}
		slog.Info("secrets: generated master key", "file", m.keyFile, "keychain_error", err)
	} else {
		slog.Info("secrets: generated master key in OS keychain")
	}
	if err := m.store.PutSetting(ctx, recipientSetting, id.Recipient().String()); err != nil {
		return err
	}
	m.mu.Lock()
	m.id, m.source = id, source
	m.mu.Unlock()
	return nil
}

func (m *Manager) unlock(key, source, stored string) error {
	id, err := age.ParseX25519Identity(strings.TrimSpace(key))
	if err != nil {
		return fmt.Errorf("invalid master key: %w", err)
	}
	if stored != "" && id.Recipient().String() != stored {
		return fmt.Errorf("master key does not match the key secrets were encrypted with")
	}
	m.mu.Lock()
	m.id, m.source = id, source
	m.mu.Unlock()
	return nil
}

// Unlock is called by POST /secrets/unlock. An empty key re-runs AutoUnlock.
func (m *Manager) Unlock(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		if err := m.AutoUnlock(ctx); err != nil {
			return err
		}
		if !m.Status().Unlocked {
			return ErrLocked
		}
		return nil
	}
	var stored string
	has, err := m.store.GetSetting(ctx, recipientSetting, &stored)
	if err != nil {
		return err
	}
	if err := m.unlock(key, "api", stored); err != nil {
		return err
	}
	if !has {
		return m.store.PutSetting(ctx, recipientSetting, m.id.Recipient().String())
	}
	return nil
}

func (m *Manager) Status() v1.SecretStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.id == nil {
		return v1.SecretStatus{}
	}
	return v1.SecretStatus{Unlocked: true, Source: m.source, Recipient: m.id.Recipient().String()}
}

func (m *Manager) identity() (*age.X25519Identity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.id == nil {
		return nil, ErrLocked
	}
	return m.id, nil
}

func validateNames(ns, name string) error {
	if !regexp.MustCompile(`^[a-z0-9_-]+$`).MatchString(ns) {
		return fmt.Errorf("invalid secret namespace %q", ns)
	}
	if !partRe.MatchString(name) {
		return fmt.Errorf("invalid secret name %q", name)
	}
	return nil
}

func (m *Manager) Set(ctx context.Context, ns, name string, value []byte, desc string) (*v1.SecretMeta, error) {
	if err := validateNames(ns, name); err != nil {
		return nil, err
	}
	id, err := m.identity()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(value); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := m.store.PutSecret(ctx, ns, name, buf.Bytes(), map[string]any{"description": desc, "size": len(value)}); err != nil {
		return nil, err
	}
	list, err := m.store.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		if s.Namespace == ns && s.Name == name {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("secret vanished")
}

// Get decrypts a secret reference (secret://ns/name).
func (m *Manager) Get(ctx context.Context, ref string) ([]byte, error) {
	ns, name, ok := ParseRef(ref)
	if !ok {
		return nil, fmt.Errorf("invalid secret reference %q", ref)
	}
	id, err := m.identity()
	if err != nil {
		return nil, err
	}
	blob, err := m.store.GetSecretBlob(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(blob), id)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", ref, err)
	}
	return io.ReadAll(r)
}

// Resolve returns the value for a secret reference, or s itself if s is not
// a reference (plain values are allowed for non-sensitive settings).
func (m *Manager) Resolve(ctx context.Context, s string) (string, error) {
	if !IsRef(s) {
		return s, nil
	}
	b, err := m.Get(ctx, s)
	return string(b), err
}

func (m *Manager) List(ctx context.Context) ([]v1.SecretMeta, error) { return m.store.ListSecrets(ctx) }

func (m *Manager) Delete(ctx context.Context, ns, name string) error {
	return m.store.DeleteSecret(ctx, ns, name)
}
