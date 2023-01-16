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
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"os"
	"os/signal"
	"time"
)

func main() {
	h, err := libp2p.New()
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	psub, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		panic(err)
	}

	topic, err := psub.Join("horse")
	if err != nil {
		panic(err)
	}

	kademlia, err := dht.New(ctx, h)
	if err != nil {
		panic(err)
	}

	//if err = kademlia.Bootstrap(ctx); err != nil {
	//	panic(err)
	//}

	rd := drouting.NewRoutingDiscovery(kademlia)

	dutil.Advertise(ctx, rd, "over here")

	p, err := peer.AddrInfoFromString("/ip4/127.0.0.1/udp/34194/quic-v1/p2p/12D3KooWKnHVkYLSD5iFPGXStsbQJbU9ySSVXPsz1xHthSi7SwR1")
	if err != nil {
		panic(err)
	}

	if err = h.Connect(ctx, *p); err != nil {
		panic(err)
	}

	_, err = topic.Subscribe()
	if err != nil {
		panic(err)
	}

	time.Sleep(10 * time.Second)

	if err = topic.Publish(ctx, []byte("Hello, here I am!")); err != nil {
		panic(err)
	}

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	fmt.Println("Received signal, shutting down...")

	// shut the node down
	if err := h.Close(); err != nil {
		panic(err)
	}
}
