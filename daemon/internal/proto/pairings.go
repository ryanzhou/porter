package proto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Peer is a paired app.
type Peer struct {
	Key      string `json:"key"` // base64 static public key
	Name     string `json:"name"`
	Added    int64  `json:"added"`
	LastSeen int64  `json:"last_seen"`
}

// Pairings is the persistent list of apps allowed to control the router.
type Pairings struct {
	mu    sync.Mutex
	path  string
	peers []Peer
}

func LoadPairings(path string) (*Pairings, error) {
	p := &Pairings{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &p.peers); err != nil {
		// Unreadable pairings must not open the router to everyone, nor
		// lock the owner out for good: start unclaimed again.
		_ = os.Rename(path, path+".bad")
		p.peers = nil
	}
	return p, nil
}

func (p *Pairings) save() error {
	b, _ := json.MarshalIndent(p.peers, "", " ")
	return config.WriteFileAtomic(p.path, b, 0o600)
}

func keyString(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

// Known reports whether the key belongs to a paired app.
func (p *Pairings) Known(key []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ks := keyString(key)
	for _, peer := range p.peers {
		kb, _ := base64.StdEncoding.DecodeString(peer.Key)
		if peer.Key == ks && bytes.Equal(kb, key) {
			return true
		}
	}
	return false
}

func (p *Pairings) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.peers)
}

func (p *Pairings) List() []Peer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Peer(nil), p.peers...)
}

// Add stores a new peer (or renames an existing one).
func (p *Pairings) Add(key []byte, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ks := keyString(key)
	now := time.Now().Unix()
	for i := range p.peers {
		if p.peers[i].Key == ks {
			p.peers[i].Name = name
			return p.save()
		}
	}
	p.peers = append(p.peers, Peer{Key: ks, Name: name, Added: now, LastSeen: now})
	return p.save()
}

// Remove deletes a peer by its base64 key.
func (p *Pairings) Remove(key string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.peers {
		if p.peers[i].Key == key {
			p.peers = append(p.peers[:i], p.peers[i+1:]...)
			return true, p.save()
		}
	}
	return false, nil
}

// Touch records that a peer connected. It is kept in memory only; the
// value is persisted the next time the list is saved.
func (p *Pairings) Touch(key []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ks := keyString(key)
	for i := range p.peers {
		if p.peers[i].Key == ks {
			p.peers[i].LastSeen = time.Now().Unix()
		}
	}
}
