// Command ledgerd runs a single ledgerline-raft node. Run three of them
// (one with -bootstrap) to form a cluster — see the repo README and
// ledgerline-docs/docker-compose.yml for a worked three-node example.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/ledgerline/ledgerline-raft/internal/httpapi"
	"github.com/ledgerline/ledgerline-raft/internal/raftnode"
)

// peerSpec is one entry of the -peers flag: nodeID=raftAddr=httpAddr.
type peerSpec struct {
	ID       string
	RaftAddr string
	HTTPAddr string
}

func parsePeers(raw string) ([]peerSpec, error) {
	var peers []peerSpec
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, "=")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid -peers entry %q, want id=raftAddr=httpAddr", entry)
		}
		peers = append(peers, peerSpec{ID: parts[0], RaftAddr: parts[1], HTTPAddr: parts[2]})
	}
	return peers, nil
}

func main() {
	nodeID := flag.String("node-id", "", "unique raft server ID for this node")
	raftBindAddr := flag.String("raft-addr", "127.0.0.1:7001", "TCP address for raft replication traffic")
	httpAddr := flag.String("http-addr", "127.0.0.1:8001", "address this node's HTTP API listens on")
	dataDir := flag.String("data-dir", "", "directory for raft log/snapshot storage (defaults to ./data/<node-id>)")
	bootstrap := flag.Bool("bootstrap", false, "bootstrap a brand-new cluster with -peers as the initial voter set (set on exactly one node)")
	peersFlag := flag.String("peers", "", "comma-separated id=raftAddr=httpAddr entries for every cluster member, including this one")
	flag.Parse()

	if *nodeID == "" {
		log.Fatal("-node-id is required")
	}
	peers, err := parsePeers(*peersFlag)
	if err != nil {
		log.Fatalf("parse -peers: %v", err)
	}
	if len(peers) == 0 {
		log.Fatal("-peers must list at least this node")
	}

	dir := *dataDir
	if dir == "" {
		dir = fmt.Sprintf("./data/%s", *nodeID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	raftPeers := make([]raftnode.Peer, 0, len(peers))
	httpByID := make(map[string]string, len(peers))
	for _, p := range peers {
		raftPeers = append(raftPeers, raftnode.Peer{ID: p.ID, RaftAddr: p.RaftAddr})
		httpByID[p.ID] = p.HTTPAddr
	}

	node, err := raftnode.Start(raftnode.Config{
		NodeID:       *nodeID,
		RaftBindAddr: *raftBindAddr,
		DataDir:      dir,
		Bootstrap:    *bootstrap,
		Peers:        raftPeers,
	})
	if err != nil {
		log.Fatalf("start raft node: %v", err)
	}

	server := httpapi.NewServer(httpapi.Config{
		Node:          node,
		PeerHTTPAddrs: httpByID,
	})

	log.Printf("ledgerd node=%s raft=%s http=%s bootstrap=%v", *nodeID, *raftBindAddr, *httpAddr, *bootstrap)
	if err := http.ListenAndServe(*httpAddr, server.Handler()); err != nil {
		log.Fatalf("http server: %v", err)
	}
}
