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
	"github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx := context.Background()

	// start a libp2p node with default settings
	node, err := libp2p.New()
	if err != nil {
		panic(err)
	}

	// print the node's listening addresses
	fmt.Println("Listen addresses:", node.Addrs())

	ps, err := pubsub.NewGossipSub(ctx, node)
	if err != nil {
		panic(err)
	}

	topic, err := ps.Join("horse")
	if err != nil {
		panic(err)
	}

	pi, err := peer.AddrInfoFromString("/ip4/127.0.0.1/tcp/27000/p2p/12D3KooWRBRtFk6xYDWUZgkoFy6GXTPDRktHQskpP5ASeZ6NacGj")
	if err != nil {
		panic(err)
	}

	err = node.Connect(ctx, *pi)
	if err != nil {
		panic(err)
	}

	kademlia, err := dht.New(ctx, node)
	if err != nil {
		panic(err)
	}

	rd := drouting.NewRoutingDiscovery(kademlia)

	dutil.Advertise(ctx, rd, "over here")

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
			} else if node.Network().Connectedness(p.ID) == network.Connected {
				continue
			}
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

	go sendFromConsole(ctx, topic)

	sub, err := topic.Subscribe()
	if err != nil {
		panic(err)
	}
	go readFromSubscription(ctx, sub)

	fmt.Println(topic.ListPeers())

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	fmt.Println("Received signal, shutting down...")

	// shut the node down
	if err := node.Close(); err != nil {
		panic(err)
	}
}

func sendFromConsole(ctx context.Context, topic *pubsub.Topic) {
	reader := bufio.NewReader(os.Stdin)
	for {
		s, err := reader.ReadString('\n')
		if err != nil {
			panic(err)
		}
		if err := topic.Publish(ctx, []byte(s)); err != nil {
			fmt.Println("Publish error: ", err)
		}
	}
}

func readFromSubscription(ctx context.Context, sub *pubsub.Subscription) {
	for {
		m, err := sub.Next(ctx)
		if err != nil {
			panic(err)
		}
		fmt.Println(m.ReceivedFrom, ": ", string(m.Message.Data))
	}
}
