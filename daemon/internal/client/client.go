// Package client is a Go implementation of the app side of the control
// protocol, used by porterctl and the tests.
package client

import (
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/ryanzhou/porter/daemon/internal/proto"
)

type Client struct {
	s      *proto.Session
	nextID atomic.Uint32
	mu     sync.Mutex
	wait   map[uint32]chan proto.Response
	// Events receives unsolicited messages; it is dropped from when full.
	Events chan RawEvent
	err    error
	closed chan struct{}
}

type RawEvent struct {
	Event string          `json:"ev"`
	Data  json.RawMessage `json:"d"`
}

// DialUnix connects to the router's local control socket.
func DialUnix(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	id, err := proto.NewIdentity()
	if err != nil {
		return nil, err
	}
	return New(proto.NewStreamConn(conn), id)
}

// New performs the handshake over an existing link.
func New(pc proto.PacketConn, id proto.Identity) (*Client, error) {
	s, err := proto.Dial(pc, id)
	if err != nil {
		pc.Close()
		return nil, err
	}
	c := &Client{s: s, wait: map[uint32]chan proto.Response{}, Events: make(chan RawEvent, 64), closed: make(chan struct{})}
	go c.read()
	return c, nil
}

// RouterKey is the router's authenticated static public key.
func (c *Client) RouterKey() []byte { return c.s.Remote }

func (c *Client) read() {
	defer close(c.closed)
	for {
		msg, err := c.s.Recv()
		if err != nil {
			c.mu.Lock()
			c.err = err
			for id, ch := range c.wait {
				close(ch)
				delete(c.wait, id)
			}
			c.mu.Unlock()
			return
		}
		var probe struct {
			Event string `json:"ev"`
		}
		_ = json.Unmarshal(msg, &probe)
		if probe.Event != "" {
			var ev RawEvent
			if json.Unmarshal(msg, &ev) == nil {
				select {
				case c.Events <- ev:
				default:
				}
			}
			continue
		}
		var raw struct {
			ID     uint32          `json:"id"`
			Result json.RawMessage `json:"r"`
			Error  *proto.Error    `json:"e"`
		}
		if json.Unmarshal(msg, &raw) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.wait[raw.ID]
		delete(c.wait, raw.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- proto.Response{ID: raw.ID, Result: raw.Result, Error: raw.Error}
		}
	}
}

// Call invokes a method. result, when not nil, receives the decoded
// result.
func (c *Client) Call(method string, params, result any) error {
	req := proto.Request{ID: c.nextID.Add(1), Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	ch := make(chan proto.Response, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.wait[req.ID] = ch
	c.mu.Unlock()
	b, _ := json.Marshal(req)
	if err := c.s.Send(b); err != nil {
		return err
	}
	resp, ok := <-ch
	if !ok {
		return errors.New("connection closed")
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result != nil {
		return json.Unmarshal(resp.Result.(json.RawMessage), result)
	}
	return nil
}

func (c *Client) Close() error { return c.s.Close() }
