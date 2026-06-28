// -------------------------------------------------------------------------------
// Oracle Watchdog - Agent Client Interfaces
//
// Author: Alex Freidah
//
// Consumer-side interfaces for the external dependencies the agent drives:
// Consul (session-key reads) and OCI (instance restarts). The agent depends on
// these narrow interfaces rather than the concrete SDK clients so the
// monitoring logic can be exercised with fakes in unit tests. The real
// implementations are thin adapters constructed by the factory functions wired
// in New().
// -------------------------------------------------------------------------------

package agent

import (
	"context"

	"github.com/afreidah/oracle-watchdog/internal/oci"

	consul "github.com/hashicorp/consul/api"
)

// ConsulClient is the subset of the Consul API the agent uses: a reachability
// probe (Leader) and a single session-key read (GetKV).
type ConsulClient interface {
	// Leader returns the current Raft leader address, used as a connectivity probe.
	Leader() (string, error)
	// GetKV reads a single key; a nil pair means the key is absent.
	GetKV(key string) (*consul.KVPair, error)
}

// InstanceRestarter is the subset of the OCI client the agent uses: a
// stop/start cycle on a single instance. *oci.Client satisfies this directly.
type InstanceRestarter interface {
	RestartInstance(ctx context.Context, instanceID, compartmentID string) error
}

// consulAdapter wraps the concrete Consul SDK client to satisfy ConsulClient.
type consulAdapter struct {
	client *consul.Client
}

func (c *consulAdapter) Leader() (string, error) {
	return c.client.Status().Leader()
}

func (c *consulAdapter) GetKV(key string) (*consul.KVPair, error) {
	pair, _, err := c.client.KV().Get(key, nil)
	return pair, err
}

// newConsulClient builds a real Consul client for the given address. It is the
// default ConsulClient factory wired into the agent in New().
func newConsulClient(address string) (ConsulClient, error) {
	cfg := consul.DefaultConfig()
	cfg.Address = address

	client, err := consul.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &consulAdapter{client: client}, nil
}

// newOCIClient builds a real OCI client. It is the default InstanceRestarter
// factory wired into the agent in New().
func newOCIClient(configPath, profile string) (InstanceRestarter, error) {
	client, err := oci.NewClient(configPath, profile)
	if err != nil {
		return nil, err
	}
	return client, nil
}
