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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"github.com/hashicorp/mdns"
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
	"strconv"
	"sync"
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
		Interest:   interest,
		Subscriber: subscriber,
	}
}

// EdgeRouter type representing an MMS edge router
type EdgeRouter struct {
	subscriptions map[string][]*Subscription // a mapping from Interest names to Subscription slices
	subMu         *sync.RWMutex              // a Mutex for locking the subscriptions map
	httpServer    *http.Server               // the http server that is used to bootstrap websocket connections
	p2pHost       *host.Host                 // the libp2p host that is used to connect to the MMS router network
	agents        map[string]*Agent          // a mapping that keeps track of agents connected to this EdgeRouter
}

func NewEdgeRouter(p2p *host.Host, listeningAddr string) *EdgeRouter {
	subs := make(map[string][]*Subscription)
	mu := &sync.RWMutex{}
	agents := make(map[string]*Agent)
	httpServer := http.Server{
		Addr: listeningAddr,
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

			m, ok := v.(map[string]interface{})
			if !ok {
				fmt.Println("Could not convert buffer to map")
				return
			}

			tmp, ok := m["register"]
			if !ok {
				if err = c.Close(websocket.StatusUnsupportedData, "First message needs to contain a 'register' object"); err != nil {
					fmt.Println(err)
				}
				return
			}

			reg, ok := tmp.(map[string]interface{})
			if !ok {
				if err = c.Close(websocket.StatusUnsupportedData, "The received 'register' object could not be parsed"); err != nil {
					fmt.Println(err)
				}
				return
			}
			// if dm has not been explicitly set, it implicitly means that it is true
			_, ok = reg["dm"]
			if !ok {
				reg["dm"] = true
			}

			var r Register
			err = mapstructure.Decode(&reg, &r)
			if err != nil {
				fmt.Println("The received message could not be decoded as a register protocol message", err)
				return
			}
			fmt.Println(r)
			a := &Agent{
				Mrn:       r.Mrn,
				Interests: r.Interests,
				Dm:        r.Dm,
				Ws:        c,
			}
			agents[a.Mrn] = a
			if r.Dm {
				b := make([]byte, 8)
				_, err = rand.Read(b)
				if err != nil {
					fmt.Println("Could not generate random nonce", err)
					return
				}
				nonce := binary.BigEndian.Uint64(b)

				auth := make(map[string]map[string]string)
				auth["authenticate"] = make(map[string]string)
				auth["authenticate"]["nonce"] = strconv.FormatUint(nonce, 10)
				// send authenticate message
				err = wsjson.Write(request.Context(), c, &auth)
				if err != nil {
					fmt.Println("Could not send auth message", err)
					return
				}
			}
			if len(r.Interests) > 0 {
				mu.Lock()
				for _, value := range r.Interests {
					subs[value] = append(subs[value], NewSubscription(value, a))
				}
				mu.Unlock()
			}
		}),
		//TLSConfig: &tls.Config{
		//	ClientAuth: tls.RequireAndVerifyClientCert,
		//	ClientCAs:  nil,
		//},
	}

	return &EdgeRouter{
		subscriptions: subs,
		subMu:         mu,
		httpServer:    &httpServer,
		p2pHost:       p2p,
		agents:        agents,
	}
}

func (er *EdgeRouter) StartEdgeRouter(ctx context.Context) {
	go func() {
		fmt.Println("Starting edge router")
		if err := er.httpServer.ListenAndServe(); err != nil {
			fmt.Println(err)
		}
	}()
	<-ctx.Done()
	fmt.Println("Shutting down edge router")
	fmt.Println("subscriptions:", er.subscriptions)
	if err := er.httpServer.Shutdown(ctx); err != nil {
		panic(err)
	}
}

func main() {
	h, err := libp2p.New()
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	_, err = pubsub.NewGossipSub(ctx, h)
	if err != nil {
		panic(err)
	}

	kademlia, err := dht.New(ctx, h)
	if err != nil {
		panic(err)
	}

	rd := drouting.NewRoutingDiscovery(kademlia)

	dutil.Advertise(ctx, rd, "over here")

	p, err := peer.AddrInfoFromString("/ip4/127.0.0.1/udp/27000/quic-v1/p2p/QmcUKyMuepvXqZhpMSBP59KKBymRNstk41qGMPj38QStfx")
	if err != nil {
		panic(err)
	}

	if err = h.Connect(ctx, *p); err != nil {
		panic(err)
	}

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)

	er := NewEdgeRouter(&h, "localhost:8080")
	go er.StartEdgeRouter(ctx)

	hst, err := os.Hostname()
	if err != nil {
		fmt.Println("Could not get hostname, shutting down", err)
		ch <- os.Interrupt
	}
	info := []string{"MMS Edge Router"}
	mdnsService, err := mdns.NewMDNSService(hst, "_mms-edgerouter_tcp", "", "", 8080, nil, info)
	if err != nil {
		fmt.Println("Could not create mDNS service, shutting down", err)
		ch <- os.Interrupt
	}
	mdnsServer, err := mdns.NewServer(&mdns.Config{Zone: mdnsService})
	if err != nil {
		fmt.Println("Could not create mDNS server, shutting down", err)
		ch <- os.Interrupt
	}
	defer mdnsServer.Shutdown()

	<-ch
	fmt.Println("Received signal, shutting down...")
	cancel()

	// shut the node down
	if err := h.Close(); err != nil {
		panic(err)
	}
}
