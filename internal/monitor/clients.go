// -------------------------------------------------------------------------------
// Oracle Watchdog - Monitor Client Interface
//
// Author: Alex Freidah
//
// Consumer-side interface for the Consul operations the monitor drives: a
// reachability probe, session create/renew/destroy, and a session-locked KV
// acquire. The monitor depends on this narrow interface rather than the
// concrete SDK client so the session lifecycle can be exercised with fakes in
// unit tests. The real implementation is a thin adapter constructed by the
// factory wired in New().
// -------------------------------------------------------------------------------

package monitor

import (
	consul "github.com/hashicorp/consul/api"
)

// ConsulSession is the subset of the Consul API the monitor uses to maintain a
// node's heartbeat session.
type ConsulSession interface {
	// Leader returns the current Raft leader address, used as a connectivity probe.
	Leader() (string, error)
	// CreateSession creates a session and returns its ID.
	CreateSession(entry *consul.SessionEntry) (string, error)
	// RenewSession renews a session; a nil entry means the session is gone.
	RenewSession(sessionID string) (*consul.SessionEntry, error)
	// DestroySession deletes a session.
	DestroySession(sessionID string) error
	// AcquireKey writes a session-locked KV pair; the bool reports whether the
	// lock was acquired.
	AcquireKey(pair *consul.KVPair) (bool, error)
}

// consulAdapter wraps the concrete Consul SDK client to satisfy ConsulSession.
type consulAdapter struct {
	client *consul.Client
}

func (c *consulAdapter) Leader() (string, error) {
	return c.client.Status().Leader()
}

func (c *consulAdapter) CreateSession(entry *consul.SessionEntry) (string, error) {
	id, _, err := c.client.Session().Create(entry, nil)
	return id, err
}

func (c *consulAdapter) RenewSession(sessionID string) (*consul.SessionEntry, error) {
	entry, _, err := c.client.Session().Renew(sessionID, nil)
	return entry, err
}

func (c *consulAdapter) DestroySession(sessionID string) error {
	_, err := c.client.Session().Destroy(sessionID, nil)
	return err
}

func (c *consulAdapter) AcquireKey(pair *consul.KVPair) (bool, error) {
	acquired, _, err := c.client.KV().Acquire(pair, nil)
	return acquired, err
}

// newConsulClient builds a real Consul client for the given address. It is the
// default ConsulSession factory wired into the monitor in New().
func newConsulClient(address string) (ConsulSession, error) {
	cfg := consul.DefaultConfig()
	cfg.Address = address

	client, err := consul.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &consulAdapter{client: client}, nil
}
