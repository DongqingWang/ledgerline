// Package raftnode wires the FSM up to a real hashicorp/raft consensus
// group: a TCP transport for inter-node replication, a BoltDB-backed log +
// stable store, and a file-based snapshot store for log compaction.
package raftnode

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/ledgerline/ledgerline-raft/internal/fsm"
)

// Peer identifies one voting member of the raft cluster.
type Peer struct {
	ID       string
	RaftAddr string // host:port for raft's own replication transport
}

// Config describes how to stand up a single raft node.
type Config struct {
	NodeID       string
	RaftBindAddr string
	DataDir      string

	// Bootstrap, when true, initializes a brand-new cluster with Peers as
	// its initial voter set. Exactly one node in a fresh cluster should pass
	// Bootstrap=true; the others join with Bootstrap=false and discover the
	// existing cluster via the same static Peers list (hashicorp/raft is
	// idempotent about re-bootstrapping onto existing state, so a restarted
	// node simply resumes from its on-disk log instead of re-bootstrapping).
	Bootstrap bool
	Peers     []Peer
}

// Node bundles the raft handle with the FSM it drives, since callers (the
// HTTP layer) need both: Raft.Apply to submit commands, FSM to read state
// and subscribe to events.
type Node struct {
	Raft *raft.Raft
	FSM  *fsm.FSM
}

func Start(cfg Config) (*Node, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(cfg.NodeID)

	addr, err := net.ResolveTCPAddr("tcp", cfg.RaftBindAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve raft bind addr: %w", err)
	}
	transport, err := raft.NewTCPTransport(cfg.RaftBindAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("create tcp transport: %w", err)
	}

	snapshots, err := raft.NewFileSnapshotStore(cfg.DataDir, 2, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("create snapshot store: %w", err)
	}

	boltStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-log.bolt"))
	if err != nil {
		return nil, fmt.Errorf("create bolt store: %w", err)
	}
	var logStore raft.LogStore = boltStore
	var stableStore raft.StableStore = boltStore

	f := fsm.New()

	r, err := raft.NewRaft(raftConfig, f, logStore, stableStore, snapshots, transport)
	if err != nil {
		return nil, fmt.Errorf("create raft node: %w", err)
	}

	if cfg.Bootstrap {
		hasState, err := raft.HasExistingState(logStore, stableStore, snapshots)
		if err != nil {
			return nil, fmt.Errorf("check existing state: %w", err)
		}
		if !hasState {
			servers := make([]raft.Server, 0, len(cfg.Peers))
			for _, p := range cfg.Peers {
				servers = append(servers, raft.Server{
					ID:      raft.ServerID(p.ID),
					Address: raft.ServerAddress(p.RaftAddr),
				})
			}
			future := r.BootstrapCluster(raft.Configuration{Servers: servers})
			if err := future.Error(); err != nil {
				return nil, fmt.Errorf("bootstrap cluster: %w", err)
			}
		}
	}

	return &Node{Raft: r, FSM: f}, nil
}

// LeaderHTTPAddr is populated by the HTTP layer's node registry; raft itself
// only knows the leader's *raft* address, not its HTTP address, so the
// server package maintains that mapping (see httpapi.Server).
func (n *Node) IsLeader() bool { return n.Raft.State() == raft.Leader }
