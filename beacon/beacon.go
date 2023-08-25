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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	peerstore "github.com/libp2p/go-libp2p/core/peer"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	pemData, err := os.ReadFile("priv.pem")
	if err != nil {
		panic(err)
	}

	keyData, _ := pem.Decode(pemData)
	ecPriv, err := x509.ParseECPrivateKey(keyData.Bytes)
	if err != nil {
		panic(err)
	}

	privEc, _, err := crypto.ECDSAKeyPairFromKey(ecPriv)
	if err != nil {
		panic(err)
	}

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

	node, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/27000", "/ip4/0.0.0.0/udp/27000/quic-v1", "/ip6/::/tcp/27000", "/ip6/::/udp/27000/quic-v1"),
		libp2p.Identity(privEc),
		libp2p.EnableNATService(),
		libp2p.EnableRelayService(),
		libp2p.EnableHolePunching(),
	)
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// print the node's listening addresses
	fmt.Println("Listen addresses:", node.Addrs())

	// print the node's PeerInfo in multiaddr format
	peerInfo := peerstore.AddrInfo{
		ID:    node.ID(),
		Addrs: node.Addrs(),
	}
	addrs, err := peerstore.AddrInfoToP2pAddrs(&peerInfo)
	fmt.Println("libp2p node addresses:", addrs)

	kademlia, err := dht.New(ctx, node, dht.Mode(dht.ModeAutoServer), dht.BootstrapPeers(beacons...))
	if err != nil {
		panic(err)
	}

	if err = kademlia.Bootstrap(ctx); err != nil {
		panic(err)
	}

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fmt.Println("Received signal, shutting down...")
	cancel()
	// shut the node down
	if err := node.Close(); err != nil {
		fmt.Println(err)
	}
}
