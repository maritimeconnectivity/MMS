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
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/mitchellh/mapstructure"
	"net/http"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
	"os"
	"os/signal"
	"sync"
	"time"
)

// type representing an MMS agent
type agent struct {
	mrn       string          // the MRN of the agent
	interests []string        // the interests that the agent wants to subscribe to
	dm        bool            // whether the agent wants to be able to receive direct messages
	ws        *websocket.Conn // the websocket connection to this agent
}

// type representing a subscription
type subscription struct {
	interest   string // the interest that the subscription is based on
	subscriber *agent // the agent that is the subscriber
}

// Register type representing the register protocol message
type Register struct {
	Mrn       string   // the MRN of the agent
	Interests []string // the interests that the agent wants to subscribe to
	Dm        bool     // whether the agent wants to be able to receive direct messages
}

func newSubscription(interest string, subscriber *agent) *subscription {
	return &subscription{
		interest,
		subscriber,
	}
}

// type representing an MMS edge router
type edgeRouter struct {
	subscriptions map[string][]*subscription // a mapping from interest names to subscription slices
	subMu         sync.Mutex                 // a Mutex for locking the subscriptions map
	httpServer    *http.Server               // the http server that is used to bootstrap websocket connections
	p2pHost       *host.Host                 // the libp2p host that is used to connect to the MMS router network
}

func (er *edgeRouter) subscribeAgentToInterest(a *agent, interest string) {
	sub := newSubscription(interest, a)
	er.subMu.Lock()
	er.subscriptions[interest] = append(er.subscriptions[interest], sub)
	er.subMu.Unlock()
}

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

	p, err := peer.AddrInfoFromString("/ip4/127.0.0.1/udp/27000/quic-v1/p2p/QmcUKyMuepvXqZhpMSBP59KKBymRNstk41qGMPj38QStfx")
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

	httpServer := http.Server{
		Addr: "localhost:8080",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			c, err := websocket.Accept(writer, request, nil)
			if err != nil {
				fmt.Println(err)
				return
			}
			defer func(c *websocket.Conn, code websocket.StatusCode, reason string) {
				err := c.Close(code, reason)
				if err != nil {
					fmt.Println("Could not close connection")
				}
			}(c, websocket.StatusInternalError, "PANIC!!!")

			var v interface{}
			err = wsjson.Read(request.Context(), c, &v)
			if err != nil {
				fmt.Println(err)
				return
			}
			m, b := v.(map[string]interface{})
			if !b {
				fmt.Println("Could not convert buffer to map")
				return
			}

			var r Register
			err = mapstructure.WeakDecode(&m, &r)
			if err != nil {
				fmt.Println("The received message could not be decoded as a register protocol message", err)
				return
			}
			fmt.Println(r)
		}),
		//TLSConfig: &tls.Config{
		//	ClientAuth: tls.RequireAndVerifyClientCert,
		//	ClientCAs:  nil,
		//},
	}

	go func() {
		err = httpServer.ListenAndServe()
		if err != nil {
			fmt.Println(err)
		}
	}()

	<-ch
	fmt.Println("Received signal, shutting down...")

	// shut the node down
	if err := h.Close(); err != nil {
		panic(err)
	}

	// shut the http server down
	if err := httpServer.Shutdown(ctx); err != nil {
		panic(err)
	}
}
