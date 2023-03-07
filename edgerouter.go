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
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/edwingeng/deque/v2"
	"github.com/golang-jwt/jwt/v4"
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

// Agent type representing an MMS agent
type Agent struct {
	Mrn         string                                   // the MRN of the Agent
	Interests   []string                                 // the Interests that the Agent wants to subscribe to
	Dm          bool                                     // whether the Agent wants to be able to receive direct messages
	Ws          *websocket.Conn                          // the websocket connection to this Agent
	dms         *deque.Deque[ProtocolMessage]            // a Deque for holding dms for the Agent
	dmMu        *sync.Mutex                              // Mutex for locking dms Deque while reading and writing
	subMessages map[string]*deque.Deque[ProtocolMessage] // a mapping from interest name to a Deque of incoming messages
	subMu       *sync.Mutex                              // Mutex for locking subMessages
}

// Register type representing the register protocol message
type Register struct {
	Mrn       string   // the MRN of the Agent
	Interests []string // the Interests that the Agent wants to subscribe to
	Dm        bool     // whether the Agent wants to be able to receive direct messages
}

type Authentication struct {
	Nonce  string `json:"nonce"`
	X5u    string `json:"x5u"`
	X5t256 string `json:"x5t#256,omitempty"`
	jwt.RegisteredClaims
}

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
	ctx           context.Context          // the main Context of the EdgeRouter
}

func NewEdgeRouter(p2p *host.Host, pubSub *pubsub.PubSub, listeningAddr string, ctx context.Context) *EdgeRouter {
	subs := make(map[string]*Subscription)
	mu := &sync.RWMutex{}
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
				dms:       deque.NewDeque[ProtocolMessage](),
				dmMu:      &sync.Mutex{},
			}

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
				nonceStr := strconv.FormatUint(nonce, 10)
				auth["authenticate"]["nonce"] = nonceStr
				// send authenticate message
				err = wsjson.Write(request.Context(), c, &auth)
				if err != nil {
					fmt.Println("Could not send auth message", err)
					return
				}

				// Receive authentication message
				err = wsjson.Read(request.Context(), c, &buf)
				if err != nil {
					fmt.Println("Could not receive authentication message", err)
					return
				}
				m, ok := buf.(map[string]interface{})
				if !ok {
					if err = c.Close(websocket.StatusUnsupportedData, "The received message could not be parsed"); err != nil {
						fmt.Println(err)
					}
					return
				}
				authn, ok := m["authentication"]
				if !ok {
					if err = c.Close(websocket.StatusUnsupportedData, "The received message does not contain an authentication attribute"); err != nil {
						fmt.Println(err)
					}
					return
				}
				// parse jwt token
				token, err := jwt.ParseWithClaims(authn.(string), &Authentication{}, func(token *jwt.Token) (interface{}, error) {
					alg := token.Method.Alg()
					if !(alg == "ES256" || alg == "ES384") {
						return nil, fmt.Errorf("token was not signed using an acceptable algorithm, the algorithm used was %s", alg)
					}

					claims, ok := token.Claims.(*Authentication)
					if !ok {
						return nil, fmt.Errorf("could not parse token claims")
					}
					// extract x5u url and GET cert chain from it
					x5u := claims.X5u
					httpClient := http.DefaultClient
					resp, err := httpClient.Get(x5u)
					if err != nil {
						return nil, fmt.Errorf("could not GET certificate chain: %w", err)
					}
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						return nil, fmt.Errorf("did not receive a certificate chain: %w", err)
					}
					if err = resp.Body.Close(); err != nil {
						return nil, fmt.Errorf("could not close response body: %w", err)
					}

					var certs []*x509.Certificate
					rest := body
					for {
						var block *pem.Block
						block, rest = pem.Decode(rest)
						if block == nil {
							break
						}
						cert, err := x509.ParseCertificate(block.Bytes)
						if err != nil {
							return nil, fmt.Errorf("could not parse certificate: %w", err)
						}
						certs = append(certs, cert)
					}
					// check revocation status of certs
					valid := true
					for i, cert := range certs {
						var issuer *x509.Certificate
						if i < len(certs)-1 {
							issuer = certs[i+1]
							// certificate is either self-signed or we haven't received the whole chain
						} else {
							issuer = cert
						}
						if err = cert.CheckSignatureFrom(issuer); err != nil {
							fmt.Println("Signature on certificate is not valid", err)
							valid = false
							break
						}
						// OCSP
						if len(cert.OCSPServer) > 0 {
							ocspUrl := cert.OCSPServer[0]
							ocspReq, err := ocsp.CreateRequest(cert, issuer, nil)
							if err != nil {
								return nil, fmt.Errorf("could not create OCSP request: %w", err)
							}
							resp, err := httpClient.Post(ocspUrl, "application/ocsp-request", bytes.NewBuffer(ocspReq))
							if err != nil {
								return nil, fmt.Errorf("could not send OCSP request: %w", err)
							}
							respBytes, err := io.ReadAll(resp.Body)
							if err != nil {
								return nil, fmt.Errorf("getting OCSP response failed: %w", err)
							}
							if err = resp.Body.Close(); err != nil {
								return nil, fmt.Errorf("could not close response body: %w", err)
							}
							ocspResp, err := ocsp.ParseResponse(respBytes, nil)
							if err != nil {
								return nil, fmt.Errorf("parsing OCSP response failed: %w", err)
							}
							if ocspResp.SerialNumber.Cmp(cert.SerialNumber) != 0 {
								fmt.Println("The serial number in the OCSP response does not correspond to the one of the certificate being checked")
								valid = false
								break
							}
							if err = ocspResp.CheckSignatureFrom(issuer); err != nil {
								fmt.Println("The signature on the OCSP response is not valid", err)
								valid = false
								break
							}
							if ocspResp.Status != ocsp.Good {
								fmt.Println("Found a certificate that was not valid")
								valid = false
								break
							}
							// CRL
						} else {
							// we can neither do OCSP nor CRL so we fail
							if len(cert.CRLDistributionPoints) < 1 {
								valid = false
								break
							}
							crlURL := cert.CRLDistributionPoints[0]
							resp, err := httpClient.Get(crlURL)
							if err != nil {
								return nil, fmt.Errorf("could not send CRL request: %w", err)
							}
							respBody, err := io.ReadAll(resp.Body)
							if err != nil {
								return nil, fmt.Errorf("getting CRL response body failed: %w", err)
							}
							if err = resp.Body.Close(); err != nil {
								return nil, fmt.Errorf("failed to close CRL response: %w body", err)
							}
							crl, err := x509.ParseRevocationList(respBody)
							if err != nil {
								return nil, fmt.Errorf("could not parse received CRL: %w", err)
							}
							if err = crl.CheckSignatureFrom(issuer); err != nil {
								fmt.Println("Signature on CRL is not valid", err)
								valid = false
								break
							}
							now := time.Now().UTC()
							for _, rev := range crl.RevokedCertificates {
								if (rev.SerialNumber.Cmp(cert.SerialNumber) == 0) && (rev.RevocationTime.UTC().Before(now)) {
									valid = false
									break
								}
							}
						}
					}

					if !valid {
						if err = c.Close(websocket.StatusPolicyViolation, "Authentication failed"); err != nil {
							fmt.Println(err)
						}
						return nil, fmt.Errorf("certificate chain could not be verified")
					}

					if claims.Nonce != nonceStr {
						return nil, fmt.Errorf("the nonce in the token does not match the one we generated")
					}

					if (claims.Subject != "") && (claims.Subject != a.Mrn) {
						return nil, fmt.Errorf("the value of the 'sub' claim does not match the MRN of from the register message: was %s, expected %s", a.Mrn, claims.Subject)
					}

					return certs[0].PublicKey.(*ecdsa.PublicKey), nil
				})
				if err != nil {
					fmt.Println("Could not parse JWT token:", err)
					if err = c.Close(websocket.StatusUnsupportedData, "The received JWT token could not be parsed"); err != nil {
						fmt.Println(err)
					}
					return
				}

				if !token.Valid {
					if err = c.Close(websocket.StatusUnsupportedData, "The received JWT token is not valid"); err != nil {
						fmt.Println(err)
					}
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
						go handleSubscription(ctx, subscription, p2p, sub)
						subs[interest] = sub
					} else {
						s.AddSubscriber(a)
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
		pubSub:        pubSub,
		ctx:           ctx,
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

func handleSubscription(ctx context.Context, sub *pubsub.Subscription, host *host.Host, subscription *Subscription) {
	for {
		select {
		case <-ctx.Done():
			sub.Cancel()
			return
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
				subscription.subsMu.RLock()
				for _, subscriber := range subscription.Subscribers {
					subscriber.subMu.Lock()
					subDeque, exists := subscriber.subMessages[subName]
					if !exists {
						subscriber.subMessages[subName] = deque.NewDeque[ProtocolMessage]()
						subDeque = subscriber.subMessages[subName]
					}
					subDeque.Enqueue(ProtocolMessage{Send: sendMessage})
					subscriber.subMu.Unlock()
				}
				subscription.subsMu.RUnlock()
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

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)

	er := NewEdgeRouter(&h, pubSub, "0.0.0.0:8080", ctx)
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
