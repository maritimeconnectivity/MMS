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

// Agent type representing an MMS agent
type Agent struct {
	Mrn       string          // the MRN of the Agent
	Interests []string        // the Interests that the Agent wants to subscribe to
	Dm        bool            // whether the Agent wants to be able to receive direct messages
	Ws        *websocket.Conn // the websocket connection to this Agent
}

// Subscription type representing a subscription
type Subscription struct {
	Interest   string // the Interest that the Subscription is based on
	Subscriber *Agent // the Agent that is the Subscriber
}

// Register type representing the register protocol message
type Register struct {
	Mrn       string   // the MRN of the Agent
	Interests []string // the Interests that the Agent wants to subscribe to
	Dm        bool     // whether the Agent wants to be able to receive direct messages
}

func NewSubscription(interest string, subscriber *Agent) *Subscription {
	return &Subscription{
		interest,
		subscriber,
	}
}

// EdgeRouter type representing an MMS edge router
type EdgeRouter struct {
	Subscriptions map[string][]*Subscription // a mapping from Interest names to Subscription slices
	SubMu         sync.Mutex                 // a Mutex for locking the Subscriptions map
	HttpServer    *http.Server               // the http server that is used to bootstrap websocket connections
	P2pHost       *host.Host                 // the libp2p host that is used to connect to the MMS router network
}

func (er *EdgeRouter) SubscribeAgentToInterest(a *Agent, interest string) {
	sub := NewSubscription(interest, a)
	er.SubMu.Lock()
	er.Subscriptions[interest] = append(er.Subscriptions[interest], sub)
	er.SubMu.Unlock()
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
