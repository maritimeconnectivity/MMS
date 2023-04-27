/*
 * Copyright 2023 Maritime Connectivity Platform Consortium
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bufio"
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	peerstore "github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"os"
	"os/signal"
	"time"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, rd := setupLibP2P(ctx)

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	for !anyConnected {
		fmt.Println("Searching for peers...")
		peerChan, err := rd.FindPeers(ctx, "over here")
		if err != nil {
			panic(err)
		}
		for p := range peerChan {
			if p.ID == node.ID() {
				continue // No self connection
			}
			fmt.Println("Peer:", p)
			err := node.Connect(ctx, p)
			if err != nil {
				fmt.Println("Failed connecting to ", p.ID.String(), ", error:", err)
			} else {
				fmt.Println("Connected to:", p.ID.String())
				anyConnected = true
			}
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("Peer discovery complete")

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	fmt.Println("Received signal, shutting down...")

	// shut the node down
	if err := node.Close(); err != nil {
		fmt.Println("libp2p node could not be shut down correctly")
	}
}

func setupLibP2P(ctx context.Context) (host.Host, *drouting.RoutingDiscovery) {
	// start a libp2p node with default settings
	node, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/0.0.0.0/udp/0/quic-v1", "/ip6/::/udp/0/quic-v1"))
	if err != nil {
		panic(err)
	}

	// print the node's listening addresses
	fmt.Println("Listen addresses:", node.Addrs())

	beacons := make([]peerstore.AddrInfo, 0, 1)
	beaconsFile, err := os.Open("beacons.txt")
	if err == nil {
		fileScanner := bufio.NewScanner(beaconsFile)
		for fileScanner.Scan() {
			addrInfo, err := peerstore.AddrInfoFromString(fileScanner.Text())
			if err != nil {
				fmt.Println("Failed to parse beacon address:", err)
				continue
			}
			beacons = append(beacons, *addrInfo)
		}
	}

	kademlia, err := dht.New(ctx, node, dht.Mode(dht.ModeAutoServer), dht.BootstrapPeers(beacons...))
	if err != nil {
		panic(err)
	}

	if err = kademlia.Bootstrap(ctx); err != nil {
		panic(err)
	}

	rd := drouting.NewRoutingDiscovery(kademlia)

	dutil.Advertise(ctx, rd, "over here")

	// print the node's PeerInfo in multiaddr format
	peerInfo := peerstore.AddrInfo{
		ID:    node.ID(),
		Addrs: node.Addrs(),
	}
	addrs, err := peerstore.AddrInfoToP2pAddrs(&peerInfo)
	fmt.Println("libp2p node addresses:", addrs)
	return node, rd
}
