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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/hashicorp/mdns"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
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

// Register type representing the register protocol message
type Register struct {
	Mrn       string   // the MRN of the Agent
	Interests []string // the Interests that the Agent wants to subscribe to
	Dm        bool     // whether the Agent wants to be able to receive direct messages
}

//type Authentication struct {
//	Nonce  string `json:"nonce"`
//	X5u    string `json:"x5u"`
//	X5t256 string `json:"x5t#256,omitempty"`
//	jwt.RegisteredClaims
//}

type ApplicationMessage struct {
	Id         string   `json:"id,omitempty"`
	Subject    string   `json:"subject"`
	Recipients []string `json:"recipients"`
	Expires    uint64   `json:"expires,omitempty"`
	Sender     string   `json:"sender,omitempty"`
	Body       string   `json:"body,omitempty"`
}

type ProtocolMessage struct {
	Send *ApplicationMessage `json:"send,omitempty"`
}

// Subscription type representing a subscription
type Subscription struct {
	Interest    string            // the Interest that the Subscription is based on
	Subscribers map[string]*Agent // the Agents that subscribe
	Topic       *pubsub.Topic     // The Topic for the subscription
	subsMu      *sync.RWMutex     // RWMutex for locking the Subscribers map
}

func NewSubscription(interest string) *Subscription {
	return &Subscription{
		Interest:    interest,
		Subscribers: make(map[string]*Agent),
		subsMu:      &sync.RWMutex{},
	}
}

func (sub *Subscription) AddSubscriber(agent *Agent) {
	sub.subsMu.Lock()
	sub.Subscribers[agent.Mrn] = agent
	sub.subsMu.Unlock()
}

func (sub *Subscription) DeleteSubscriber(agent *Agent) {
	sub.subsMu.Lock()
	delete(sub.Subscribers, agent.Mrn)
	sub.subsMu.Unlock()
}

// EdgeRouter type representing an MMS edge router
type EdgeRouter struct {
	subscriptions map[string]*Subscription // a mapping from Interest names to Subscription slices
	subMu         *sync.RWMutex            // a Mutex for locking the subscriptions map
	httpServer    *http.Server             // the http server that is used to bootstrap websocket connections
	p2pHost       *host.Host               // the libp2p host that is used to connect to the MMS router network
	pubSub        *pubsub.PubSub           // a PubSub instance for the EdgeRouter
	rmqConnection *amqp.Connection         // the connection to the RabbitMQ message broker
	ctx           context.Context          // the main Context of the EdgeRouter
}

func NewEdgeRouter(p2p *host.Host, pubSub *pubsub.PubSub, listeningAddr string, rmqConnection *amqp.Connection, ctx context.Context) *EdgeRouter {
	subs := make(map[string]*Subscription)
	mu := &sync.RWMutex{}
	httpServer := http.Server{
		Addr:    listeningAddr,
		Handler: handleHttpConnection(p2p, pubSub, rmqConnection, mu, subs, ctx),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.VerifyClientCertIfGiven,
			ClientCAs:             nil,
			MinVersion:            tls.VersionTLS13,
			VerifyPeerCertificate: verifyAgentCertificate(),
		},
	}

	return &EdgeRouter{
		subscriptions: subs,
		subMu:         mu,
		httpServer:    &httpServer,
		p2pHost:       p2p,
		pubSub:        pubSub,
		rmqConnection: rmqConnection,
		ctx:           ctx,
	}
}

func (er *EdgeRouter) StartEdgeRouter(ctx context.Context) {
	rmqChannel, err := er.rmqConnection.Channel()
	if err != nil {
		panic(err)
	}
	rmqChannel.Close()
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

		ch, err := rmqConnection.Channel()
		if err != nil {
			fmt.Println("Could not make a channel to RabbitMQ for agent", err)
			return
		}

		q, err := ch.QueueDeclare(
			a.Mrn,
			true,
			false,
			true,
			false,
			nil,
		)
		if err != nil {
			fmt.Println("Could not declare a queue for agent", err)
			return
		}

		if r.Dm && len(request.TLS.VerifiedChains) > 0 {
			err = ch.QueueBind(
				q.Name,
				a.Mrn,
				"direct_messages",
				false,
				nil,
			)
			if err != nil {
				fmt.Println("Could not bind direct messages to agent queue:", err)
				return
			}
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
					sub.AddSubscriber(a)
					sub.Topic = topic
					subscription, err := topic.Subscribe()
					if err != nil {
						panic(err)
					}
					go handleSubscription(ctx, subscription, p2p, sub, rmqConnection)
					subs[interest] = sub
				} else {
					s.AddSubscriber(a)
				}
				err = ch.QueueBind(
					q.Name,
					interest,
					"subscriptions",
					false,
					nil,
				)
				if err != nil {
					fmt.Println("Could not subscribe agent to topic:", err)
				}
			}
			mu.Unlock()
			// Make sure to delete agent after it disconnects
			defer func() {
				for _, interest := range a.Interests {
					subs[interest].DeleteSubscriber(a)
				}
			}()
		}

		for {
			err = wsjson.Read(request.Context(), c, &buf)
			if err != nil {
				fmt.Println("Could not read message from agent:", err)
				break
			}
		}
	}
}

func verifyAgentCertificate() func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		// we did not receive a certificate from the client, so we just return early
		if len(rawCerts) == 0 || len(verifiedChains) == 0 {
			return nil
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
			ocspResp, err := ocsp.ParseResponse(respBytes, nil)
			if err != nil {
				return fmt.Errorf("parsing OCSP response failed: %w", err)
			}
			if ocspResp.SerialNumber.Cmp(clientCert.SerialNumber) != 0 {
				return fmt.Errorf("the serial number in the OCSP response does not correspond to the serial number of the certificate being checked")
			}
			if err = ocspResp.CheckSignatureFrom(issuingCert); err != nil {
				return fmt.Errorf("the signature on the OCSP response is not valid: %w", err)
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
				err = ch.PublishWithContext(
					ctx,
					"subscriptions",
					subName,
					false,
					false,
					amqp.Publishing{
						ContentType: "application/json",
						Body:        m.Data,
					},
				)
				if err != nil {
					fmt.Println("Could not publish subscription message", err)
				}
			}
		}
	}
}

func main() {
	h, err := libp2p.New()
	if err != nil {
		panic(err)
	}
	defer func() {
		// shut the node down
		if err := h.Close(); err != nil {
			fmt.Println("Could not close p2p host")
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())

	pubSub, err := pubsub.NewGossipSub(ctx, h)
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

	rmq, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	if err != nil {
		panic(err)
	}
	defer rmq.Close()

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)

	er := NewEdgeRouter(&h, pubSub, "0.0.0.0:8080", rmq, ctx)
	go er.StartEdgeRouter(ctx)

	hst, err := os.Hostname()
	if err != nil {
		fmt.Println("Could not get hostname, shutting down", err)
		ch <- os.Interrupt
	}
	info := []string{"MMS Edge Router"}
	mdnsService, err := mdns.NewMDNSService(hst, "_mms-edgerouter._tcp", "", "", 8080, nil, info)
	if err != nil {
		fmt.Println("Could not create mDNS service, shutting down", err)
		ch <- os.Interrupt
	}
	mdnsServer, err := mdns.NewServer(&mdns.Config{Zone: mdnsService})
	if err != nil {
		fmt.Println("Could not create mDNS server, shutting down", err)
		ch <- os.Interrupt
	}
	defer func(mdnsServer *mdns.Server) {
		err := mdnsServer.Shutdown()
		if err != nil {
			fmt.Println("Shutting down mDNS server failed", err)
		}
	}(mdnsServer)

	<-ch
	fmt.Println("Received signal, shutting down...")
	cancel()
}
