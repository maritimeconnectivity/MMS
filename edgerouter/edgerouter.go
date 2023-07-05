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
	"fmt"
	"github.com/hashicorp/mdns"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"golang.org/x/crypto/ocsp"
	"google.golang.org/protobuf/proto"
	"io"
	"maritimeconnectivity.net/mms-router/generated/mmtp"
	"net/http"
	"nhooyr.io/websocket"
	"os"
	"os/signal"
	"sync"
	"time"
)

// Agent type representing a connected Edge Router
type Agent struct {
	Mrn            string                       // the MRN of the Agent
	Interests      []string                     // the Interests that the Agent wants to subscribe to
	Messages       map[string]*mmtp.MmtpMessage // the incoming messages for this Agent
	msgMu          *sync.RWMutex                // RWMutex for locking the Messages map
	reconnectToken string                       // token for reconnecting to a previous session
}

func (er *Agent) QueueMessage(mmtpMessage *mmtp.MmtpMessage) error {
	uUid := mmtpMessage.GetUuid()
	if uUid == "" {
		return fmt.Errorf("the message does not contain a UUID")
	}
	er.msgMu.Lock()
	er.Messages[uUid] = mmtpMessage
	er.msgMu.Unlock()
	return nil
}

func (er *Agent) BulkQueueMessages(mmtpMessages []*mmtp.MmtpMessage) {
	er.msgMu.Lock()
	for _, message := range mmtpMessages {
		er.Messages[message.Uuid] = message
	}
	er.msgMu.Unlock()
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
	subscriptions   map[string]*Subscription // a mapping from Interest names to Subscription slices
	subMu           *sync.RWMutex            // a Mutex for locking the subscriptions map
	agents          map[string]*Agent        // a map of connected Agents
	agentsMu        *sync.RWMutex            // a Mutex for locking the agents map
	httpServer      *http.Server             // the http server that is used to bootstrap websocket connections
	incomingChannel chan *mmtp.MmtpMessage   // channel for incoming messages
	outgoingChannel chan *mmtp.MmtpMessage   // channel for outgoing messages
	ctx             context.Context          // the main Context of the EdgeRouter
}

func NewEdgeRouter(listeningAddr string, incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan *mmtp.MmtpMessage, ctx context.Context) *EdgeRouter {
	subs := make(map[string]*Subscription)
	mu := &sync.RWMutex{}
	agents := make(map[string]*Agent)
	agentsMu := &sync.RWMutex{}
	httpServer := http.Server{
		Addr:    listeningAddr,
		Handler: handleHttpConnection(incomingChannel, outgoingChannel, subs, mu, ctx),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.VerifyClientCertIfGiven,
			ClientCAs:             nil,
			MinVersion:            tls.VersionTLS13,
			VerifyPeerCertificate: verifyAgentCertificate(),
		},
	}

	return &EdgeRouter{
		subscriptions:   subs,
		subMu:           mu,
		agents:          agents,
		agentsMu:        agentsMu,
		httpServer:      &httpServer,
		incomingChannel: incomingChannel,
		outgoingChannel: outgoingChannel,
		ctx:             ctx,
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

func handleHttpConnection(incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, subs map[string]*Subscription, mu *sync.RWMutex, ctx context.Context) http.HandlerFunc {
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

func handleIncomingMessages(ctx context.Context, edgeRouter *EdgeRouter) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for incomingMessage := range edgeRouter.incomingChannel {
				switch incomingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						appMsg := incomingMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
						if appMsg == nil {
							continue
						}
						switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
						case *mmtp.ApplicationMessageHeader_Subject:
							{
								edgeRouter.subMu.RLock()
								for _, subscriber := range edgeRouter.subscriptions[subjectOrRecipient.Subject].Subscribers {
									if err := subscriber.QueueMessage(incomingMessage); err != nil {
										fmt.Println("Could not queue message:", err)
										continue
									}
								}
								edgeRouter.subMu.RUnlock()
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								edgeRouter.agentsMu.RLock()
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									err := edgeRouter.agents[recipient].QueueMessage(incomingMessage)
									if err != nil {
										fmt.Println("Could not queue message for Edge Router:", err)
									}
								}
								edgeRouter.agentsMu.RUnlock()
							}
						}
					}
				default:
					continue
				}
			}
		}
	}
}

func handleOutgoingMessages(ctx context.Context, edgeRouter *EdgeRouter) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for outgoingMessage := range edgeRouter.outgoingChannel {
				switch outgoingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						appMsg := outgoingMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
						if appMsg == nil {
							continue
						}
						msgBytes, err := proto.Marshal(outgoingMessage)
						if err != nil {
							fmt.Println("Could not marshal outgoing message:", err)
							continue
						}
						switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
						case *mmtp.ApplicationMessageHeader_Subject:
						case *mmtp.ApplicationMessageHeader_Recipients:
							// Everything should just be sent to the Router
						}
					}
				default:
					continue
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

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)

	incomingChannel := make(chan *mmtp.MmtpMessage)
	outgoingChannel := make(chan *mmtp.MmtpMessage)

	er := NewEdgeRouter("0.0.0.0:8080", incomingChannel, outgoingChannel, ctx)
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
