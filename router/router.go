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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	peerstore "github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/mitchellh/mapstructure"
	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/crypto/ocsp"
	"io"
	"net/http"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"
)

type EdgeRouter struct {
	Mrn       string          // the MRN of the EdgeRouter
	Interests []string        // the Interests that the EdgeRouter wants to subscribe to
	Ws        *websocket.Conn // the websocket connection to this EdgeRouter
}

// Register type representing the register protocol message
type Register struct {
	Mrn       string   // the MRN of the EdgeRouter
	Interests []string // the Interests that the EdgeRouter wants to subscribe to
}

type ApplicationMessage struct {
	Id         string   `json:"id,omitempty"`
	Subject    string   `json:"subject"`
	Recipients []string `json:"recipients"`
	Expires    int64    `json:"expires,omitempty"`
	Sender     string   `json:"sender,omitempty"`
	Body       string   `json:"body,omitempty"`
}

type ProtocolMessage struct {
	Send    *ApplicationMessage `json:"send,omitempty"`
	Receive interface{}         `json:"receive,omitempty"`
}

// Subscription type representing a subscription
type Subscription struct {
	Interest    string                 // the Interest that the Subscription is based on
	Subscribers map[string]*EdgeRouter // the EdgeRouters that subscribe
	Topic       *pubsub.Topic          // The Topic for the subscription
	subsMu      *sync.RWMutex          // RWMutex for locking the Subscribers map
}

func NewSubscription(interest string) *Subscription {
	return &Subscription{
		Interest:    interest,
		Subscribers: make(map[string]*EdgeRouter),
		subsMu:      &sync.RWMutex{},
	}
}

func (sub *Subscription) AddSubscriber(edgeRouter *EdgeRouter) {
	sub.subsMu.Lock()
	sub.Subscribers[edgeRouter.Mrn] = edgeRouter
	sub.subsMu.Unlock()
}

func (sub *Subscription) DeleteSubscriber(edgeRouter *EdgeRouter) {
	sub.subsMu.Lock()
	delete(sub.Subscribers, edgeRouter.Mrn)
	sub.subsMu.Unlock()
}

// MMSRouter type representing an MMS edge router
type MMSRouter struct {
	subscriptions map[string]*Subscription // a mapping from Interest names to Subscription slices
	subMu         *sync.RWMutex            // a Mutex for locking the subscriptions map
	httpServer    *http.Server             // the http server that is used to bootstrap websocket connections
	p2pHost       *host.Host               // the libp2p host that is used to connect to the MMS router network
	pubSub        *pubsub.PubSub           // a PubSub instance for the EdgeRouter
	rmqConnection *amqp.Connection         // the connection to the RabbitMQ message broker
	ctx           context.Context          // the main Context of the EdgeRouter
}

func NewMMSRouter(p2p *host.Host, pubSub *pubsub.PubSub, listeningAddr string, rmqConnection *amqp.Connection, ctx context.Context) *MMSRouter {
	subs := make(map[string]*Subscription)
	mu := &sync.RWMutex{}
	httpServer := http.Server{
		Addr:    listeningAddr,
		Handler: handleHttpConnection(p2p, pubSub, rmqConnection, mu, subs, ctx),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.RequireAndVerifyClientCert,
			ClientCAs:             nil, // this should come from a file containing the CAs we trust
			MinVersion:            tls.VersionTLS13,
			VerifyPeerCertificate: verifyEdgeRouterCertificate(),
		},
	}

	return &MMSRouter{
		subscriptions: subs,
		subMu:         mu,
		httpServer:    &httpServer,
		p2pHost:       p2p,
		pubSub:        pubSub,
		rmqConnection: rmqConnection,
		ctx:           ctx,
	}
}

func (r *MMSRouter) StartRouter(ctx context.Context) {
	rmqChannel, err := r.rmqConnection.Channel()
	if err != nil {
		panic(err)
	}
	defer rmqChannel.Close()
	err = rmqChannel.ExchangeDeclare(
		"subscriptions",
		"direct",
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		panic(err)
	}
	err = rmqChannel.ExchangeDeclare(
		"direct_messages",
		"direct",
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		panic(err)
	}
	go func() {
		fmt.Println("Starting MMS Router")
		if err := r.httpServer.ListenAndServe(); err != nil {
			fmt.Println(err)
		}
	}()
	<-ctx.Done()
	fmt.Println("Shutting down MMS router")
	fmt.Println("subscriptions:", r.subscriptions)
	if err := r.httpServer.Shutdown(ctx); err != nil {
		panic(err)
	}
}

func handleHttpConnection(p2p *host.Host, pubSub *pubsub.PubSub, rmqConnection *amqp.Connection, mu *sync.RWMutex, subs map[string]*Subscription, ctx context.Context) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		c, err := websocket.Accept(writer, request, nil)
		if err != nil {
			fmt.Println("Could not establish websocket connection", err)
			return
		}
		defer func(c *websocket.Conn, code websocket.StatusCode, reason string) {
			err := c.Close(code, reason)
			if err != nil {
				fmt.Println("Could not close connection:", err)
			}
		}(c, websocket.StatusInternalError, "PANIC!!!")

		var buf interface{}
		err = wsjson.Read(request.Context(), c, &buf)
		if err != nil {
			fmt.Println(err)
			return
		}

		m, ok := buf.(map[string]interface{})
		if !ok {
			fmt.Println("Could not convert buffer to map")
			return
		}

		tmp, ok := m["register"]
		if !ok {
			if err = c.Close(websocket.StatusUnsupportedData, "First message needs to contain e 'register' object"); err != nil {
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
			fmt.Println("The received message could not be decoded as e register protocol message", err)
			return
		}
		fmt.Println(r)

		e := &EdgeRouter{
			Mrn:       r.Mrn,
			Interests: r.Interests,
			Ws:        c,
		}

		ch, err := rmqConnection.Channel()
		if err != nil {
			fmt.Println("Could not make e channel to RabbitMQ for edge router", err)
			return
		}

		q, err := ch.QueueDeclare(
			e.Mrn,
			true,
			false,
			false,
			false,
			nil,
		)
		if err != nil {
			fmt.Println("Could not declare e queue for edge router", err)
			return
		}

		err = ch.QueueBind(
			q.Name,
			e.Mrn,
			"direct_messages",
			false,
			nil,
		)
		if err != nil {
			fmt.Println("Could not bind direct messages to edge router queue:", err)
			return
		}

		if len(r.Interests) > 0 {
			mu.Lock()
			for _, interest := range r.Interests {
				s, exists := subs[interest]
				if !exists {
					sub := NewSubscription(interest)
					topic, err := pubSub.Join(interest)
					if err != nil {
						panic(err)
					}
					sub.AddSubscriber(e)
					sub.Topic = topic
					subscription, err := topic.Subscribe()
					if err != nil {
						panic(err)
					}
					go handleSubscription(ctx, subscription, p2p, sub, rmqConnection)
					subs[interest] = sub
				} else {
					s.AddSubscriber(e)
				}
				err = ch.QueueBind(
					q.Name,
					interest,
					"subscriptions",
					false,
					nil,
				)
				if err != nil {
					fmt.Println("Could not subscribe edge router to topic:", err)
				}
			}
			mu.Unlock()
			// Make sure to delete edge router after it disconnects
			defer func() {
				for _, interest := range e.Interests {
					subs[interest].DeleteSubscriber(e)
				}
			}()
		}

		for {
			var protoMessage ProtocolMessage
			err = wsjson.Read(request.Context(), c, &protoMessage)
			if err != nil {
				fmt.Println("Could not read message from edge router:", err)
				break
			}

			if protoMessage.Send != nil {
				sendMsg := protoMessage.Send

				// for now, we just assume that it is e subject cast message
				subject := sendMsg.Subject
				subscription, ok := subs[subject]
				if !ok {
					fmt.Printf("Nobody is subscribed to the subject %s\n", subject)
					continue
				}

				newProtMsg := ProtocolMessage{Send: sendMsg}

				msgJson, err := json.Marshal(newProtMsg)
				if err != nil {
					fmt.Println("Could not JSON marshal the message", err)
					continue
				}

				err = subscription.Topic.Publish(request.Context(), msgJson)
				if err != nil {
					fmt.Println("Could not publish message to topic", err)
				}

				msg := amqp.Publishing{
					ContentType: "application/json",
					Body:        msgJson,
					AppId:       q.Name, // we set this, so we later can filter out messages pushed by ourselves
				}

				// if the message has e TTL we should also set it on the message being published
				if sendMsg.Expires > 0 {
					now := time.Now().Unix()
					ttl := sendMsg.Expires - now
					msg.Expiration = strconv.FormatInt(ttl, 10)
				}

				err = ch.PublishWithContext(
					ctx,
					"subscriptions",
					subject,
					false,
					false,
					msg,
				)
				if err != nil {
					fmt.Println("Could not publish subscription message:", err)
				}
			}

			if protoMessage.Receive != nil {
				msgs := make([]ProtocolMessage, 0, 10)
				msgChan, err := ch.Consume(q.Name, q.Name, true, true, true, false, nil)
				if err != nil {
					fmt.Println("Could not get messages from queue:", err)
					continue
				}

				// make a WaitGroup to make sure that messages from the message queue are written to the slice before continuing
				var wg sync.WaitGroup
				wg.Add(1)

				go func() {
					for delivery := range msgChan {
						// we don't care about messages that were published by ourselves
						if delivery.AppId == q.Name {
							continue
						}
						var protoMessage ProtocolMessage
						err = json.Unmarshal(delivery.Body, &protoMessage)
						if err != nil {
							fmt.Println("Could not unmarshal message:", err)
							continue
						}
						msgs = append(msgs, protoMessage)
					}
					wg.Done()
				}()

				err = ch.Cancel(q.Name, false) // cancel consumer immediately to get current messages in queue
				if err != nil {
					fmt.Println("Could not cancel consumer:", err)
					continue
				}

				wg.Wait()

				err = wsjson.Write(request.Context(), c, msgs)
				if err != nil {
					fmt.Println("Could not send message to edge router over websocket:", err)
				}
			}
		}
	}
}

func verifyEdgeRouterCertificate() func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		// we did not receive a certificate from the client, so we just return early
		if len(rawCerts) == 0 || len(verifiedChains) == 0 {
			return fmt.Errorf("client did not send a valid certificate")
		}

		clientCert := verifiedChains[0][0]
		issuingCert := verifiedChains[0][1]

		httpClient := http.DefaultClient
		if len(clientCert.OCSPServer) > 0 {
			ocspUrl := clientCert.OCSPServer[0]
			ocspReq, err := ocsp.CreateRequest(clientCert, issuingCert, nil)
			if err != nil {
				return fmt.Errorf("could not create OCSP request for the given client cert: %w", err)
			}
			resp, err := httpClient.Post(ocspUrl, "application/ocsp-request", bytes.NewBuffer(ocspReq))
			if err != nil {
				return fmt.Errorf("could not send OCSP request: %w", err)
			}
			respBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("getting OCSP response failed: %w", err)
			}
			if err = resp.Body.Close(); err != nil {
				return fmt.Errorf("could not close response body: %w", err)
			}
			ocspResp, err := ocsp.ParseResponse(respBytes, issuingCert)
			if err != nil {
				return fmt.Errorf("parsing OCSP response failed: %w", err)
			}
			if ocspResp.SerialNumber.Cmp(clientCert.SerialNumber) != 0 {
				return fmt.Errorf("the serial number in the OCSP response does not correspond to the serial number of the certificate being checked")
			}
			if ocspResp.Certificate == nil {
				if err = ocspResp.CheckSignatureFrom(issuingCert); err != nil {
					return fmt.Errorf("the signature on the OCSP response is not valid: %w", err)
				}
			}
			if ocspResp.Status != ocsp.Good {
				return fmt.Errorf("the given client certificate has been revoked")
			}
		} else if len(clientCert.CRLDistributionPoints) > 0 {
			crlURL := clientCert.CRLDistributionPoints[0]
			resp, err := httpClient.Get(crlURL)
			if err != nil {
				return fmt.Errorf("could not send CRL request: %w", err)
			}
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("getting CRL response body failed: %w", err)
			}
			if err = resp.Body.Close(); err != nil {
				return fmt.Errorf("failed to close CRL response: %w body", err)
			}
			crl, err := x509.ParseRevocationList(respBody)
			if err != nil {
				return fmt.Errorf("could not parse received CRL: %w", err)
			}
			if err = crl.CheckSignatureFrom(issuingCert); err != nil {
				return fmt.Errorf("signature on CRL is not valid: %w", err)
			}
			now := time.Now().UTC()
			for _, rev := range crl.RevokedCertificates {
				if (rev.SerialNumber.Cmp(clientCert.SerialNumber) == 0) && (rev.RevocationTime.UTC().Before(now)) {
					return fmt.Errorf("the given client certificate has been revoked")
				}
			}
		} else {
			return fmt.Errorf("was not able to check revocation status of client certificate")
		}

		return nil
	}
}

func handleSubscription(ctx context.Context, sub *pubsub.Subscription, host *host.Host, subscription *Subscription, rmqConnection *amqp.Connection) {
	ch, err := rmqConnection.Channel()
	if err != nil {
		panic(err)
	}
	defer ch.Close()
	for {
		select {
		case <-ctx.Done():
			sub.Cancel()
			break
		default:
			m, err := sub.Next(ctx)
			if err != nil {
				fmt.Println("Could not get message from subscription:", err)
				continue
			}
			if m.GetFrom() != (*host).ID() {
				var protoMessage ProtocolMessage
				if err = json.Unmarshal(m.Data, &protoMessage); err != nil {
					fmt.Println("Could not unmarshal received message as an protocol message:", err)
					continue
				}
				sendMessage := protoMessage.Send
				if sendMessage == nil {
					fmt.Println("The received protocol message did not contain a send application message")
					continue
				}
				uid, err := uuid.Parse(sendMessage.Id)
				if err != nil {
					fmt.Println("The ID of the message is not a valid UUID:", err)
					continue
				}
				if uid.Version() != 4 {
					fmt.Println("The ID of the message is not a valid version 4 UUID")
					continue
				}
				subName := subscription.Interest
				if sendMessage.Subject != subName {
					fmt.Println("The subject of the message does not match the name of the subscription")
					continue
				}

				msg := amqp.Publishing{
					ContentType: "application/json",
					Body:        m.Data,
				}

				// if the received message has a TTL we should set it on the message being published
				if sendMessage.Expires != 0 {
					now := time.Now().Unix()
					ttl := sendMessage.Expires - now
					msg.Expiration = strconv.FormatInt(ttl, 10)
				}

				err = ch.PublishWithContext(
					ctx,
					"subscriptions",
					subName,
					false,
					false,
					msg,
				)
				if err != nil {
					fmt.Println("Could not publish subscription message", err)
				}
			}
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, rd := setupLibP2P(ctx)

	pubSub, err := pubsub.NewGossipSub(ctx, node)
	if err != nil {
		panic(err)
	}

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	attempts := 0
	for !anyConnected && attempts < 20 {
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
		attempts++
		time.Sleep(2 * time.Second)
	}
	fmt.Println("Peer discovery complete")

	rmq, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	if err != nil {
		panic("Could not connect to RabbitMQ")
	}
	defer rmq.Close()

	router := NewMMSRouter(&node, pubSub, "0.0.0.0:8080", rmq, ctx)
	go router.StartRouter(ctx)

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
