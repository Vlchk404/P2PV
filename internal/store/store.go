// Package store persists what a client needs to keep between runs: its
// long-term identity key, and the networks it has joined.
//
// The password is never written to disk. What is stored is the derived
// pre-shared key, which is what the protocol actually needs -- so reconnecting
// to a remembered network skips the Argon2id pass and still never keeps the
// password around.
package store

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Vlchk404/p2pv/internal/netid"
	"github.com/Vlchk404/p2pv/internal/session"
)

const (
	identityFile = "identity.key"
	networksFile = "networks.json"
)

// Dir returns the P2PV configuration directory, creating it if needed.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "P2PV")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// LoadOrCreateIdentity returns this device's long-term key pair, generating and
// saving one on first run.
//
// The identity is what gives a device a stable peer ID and therefore a stable
// virtual IP. Losing the file means rejoining as a new device with a new
// address, which is why it is written before it is used.
func LoadOrCreateIdentity() (*session.Identity, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, identityFile)

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(raw) != session.KeyLen {
			return nil, fmt.Errorf("store: %s is %d bytes, expected %d", path, len(raw), session.KeyLen)
		}
		var priv [session.KeyLen]byte
		copy(priv[:], raw)
		return session.IdentityFromPrivate(priv)

	case errors.Is(err, os.ErrNotExist):
		id, err := session.NewIdentity()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, id.Private[:], 0o600); err != nil {
			return nil, fmt.Errorf("store: save identity: %w", err)
		}
		return id, nil

	default:
		return nil, err
	}
}

// SavedNetwork is a remembered network.
type SavedNetwork struct {
	Name string `json:"name"`
	ID   string `json:"id"`  // hex
	PSK  string `json:"psk"` // hex
}

// Network rebuilds a netid.Network from the saved form.
func (s SavedNetwork) Network() (*netid.Network, error) {
	idRaw, err := hex.DecodeString(s.ID)
	if err != nil || len(idRaw) != netid.KeyLen {
		return nil, fmt.Errorf("store: network %q has a corrupt ID", s.Name)
	}
	pskRaw, err := hex.DecodeString(s.PSK)
	if err != nil || len(pskRaw) != netid.KeyLen {
		return nil, fmt.Errorf("store: network %q has a corrupt key", s.Name)
	}
	var id, psk [netid.KeyLen]byte
	copy(id[:], idRaw)
	copy(psk[:], pskRaw)
	return netid.FromPSK(s.Name, id, psk), nil
}

// SaveNetwork remembers a network so it can be rejoined by name alone.
func SaveNetwork(n *netid.Network) error {
	all, err := LoadNetworks()
	if err != nil {
		return err
	}
	all[n.Name] = SavedNetwork{
		Name: n.Name,
		ID:   hex.EncodeToString(n.ID[:]),
		PSK:  hex.EncodeToString(n.PSK[:]),
	}
	return writeNetworks(all)
}

// ForgetNetwork removes a remembered network.
func ForgetNetwork(name string) error {
	all, err := LoadNetworks()
	if err != nil {
		return err
	}
	if _, ok := all[name]; !ok {
		return fmt.Errorf("store: no saved network named %q", name)
	}
	delete(all, name)
	return writeNetworks(all)
}

// LoadNetworks returns every remembered network, keyed by name.
func LoadNetworks() (map[string]SavedNetwork, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, networksFile))
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]SavedNetwork), nil
	}
	if err != nil {
		return nil, err
	}
	var all map[string]SavedNetwork
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("store: %s is corrupt: %w", networksFile, err)
	}
	if all == nil {
		all = make(map[string]SavedNetwork)
	}
	return all, nil
}

// LoadNetwork returns one remembered network by name.
func LoadNetwork(name string) (*netid.Network, error) {
	all, err := LoadNetworks()
	if err != nil {
		return nil, err
	}
	saved, ok := all[name]
	if !ok {
		return nil, fmt.Errorf("store: no saved network named %q", name)
	}
	return saved.Network()
}

// NetworkNames lists remembered networks alphabetically.
func NetworkNames() ([]string, error) {
	all, err := LoadNetworks()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func writeNetworks(all map[string]SavedNetwork) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, networksFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
