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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	peerstore "github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
	"github.com/maritimeconnectivity/MMS/consumer"
	"github.com/maritimeconnectivity/MMS/mmtp"
	"github.com/maritimeconnectivity/MMS/utils/auth"
	"github.com/maritimeconnectivity/MMS/utils/errMsg"
	"github.com/maritimeconnectivity/MMS/utils/revocation"
	"github.com/maritimeconnectivity/MMS/utils/rw"
	"google.golang.org/protobuf/proto"
	"log"
	"net/http"
	"nhooyr.io/websocket"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	WsReadLimit      int64         = 1 << 20             // 1 MiB = 1048576 B
	MessageSizeLimit int           = 50 * (1 << 10)      // 50 KiB = 51200 B
	ExpirationLimit  time.Duration = time.Hour * 24 * 30 // 30 days
	ChannelBufSize   int           = 1048576
)

// EdgeRouter type representing a connected Edge Router
type EdgeRouter struct {
	consumer.Consumer // A base struct that applies both to Agent and Edge Router consumers
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
	subscriptions   map[string]*Subscription // a mapping from Interest names to Subscription slices
	subMu           *sync.RWMutex            // a Mutex for locking the subscriptions map
	edgeRouters     map[string]*EdgeRouter   // a map of connected EdgeRouters
	erMu            *sync.RWMutex            // a Mutex for locking the edgeRouters map
	httpServer      *http.Server             // the http server that is used to bootstrap websocket connections
	p2pHost         *host.Host               // the libp2p host that is used to connect to the MMS router network
	pubSub          *pubsub.PubSub           // a PubSub instance for the EdgeRouter
	topicHandles    map[string]*pubsub.Topic // a map of Topic handles
	incomingChannel chan *mmtp.MmtpMessage   // channel for incoming messages
	outgoingChannel chan *mmtp.MmtpMessage   // channel for outgoing messages
}

func NewMMSRouter(p2p *host.Host, pubSub *pubsub.PubSub, listeningAddr string, incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan *mmtp.MmtpMessage, ctx context.Context, wg *sync.WaitGroup, clientCAs *string) (*MMSRouter, error) {
	subs := make(map[string]*Subscription)
	subMu := &sync.RWMutex{}
	edgeRouters := make(map[string]*EdgeRouter)
	erMu := &sync.RWMutex{}
	topicHandles := make(map[string]*pubsub.Topic)

	var certPool *x509.CertPool = nil
	if *clientCAs != "" {
		certPool = x509.NewCertPool()
		certFile, err := os.ReadFile(*clientCAs)
		if err != nil {
			return nil, fmt.Errorf("could not read the given client CA file")
		}
		if !certPool.AppendCertsFromPEM(certFile) {
			return nil, fmt.Errorf("could not read the given client CA file")
		}
	}

	httpServer := http.Server{
		Addr:    listeningAddr,
		Handler: handleHttpConnection(p2p, pubSub, incomingChannel, outgoingChannel, subs, subMu, edgeRouters, erMu, topicHandles, ctx, wg),
		TLSConfig: &tls.Config{
			ClientAuth:            tls.RequireAndVerifyClientCert,
			ClientCAs:             certPool, // this should come from a file containing the CAs we trust
			MinVersion:            tls.VersionTLS12,
			VerifyPeerCertificate: verifyEdgeRouterCertificate(),
		},
	}

	return &MMSRouter{
		subscriptions:   subs,
		subMu:           subMu,
		edgeRouters:     edgeRouters,
		erMu:            erMu,
		httpServer:      &httpServer,
		p2pHost:         p2p,
		pubSub:          pubSub,
		topicHandles:    topicHandles,
		incomingChannel: incomingChannel,
		outgoingChannel: outgoingChannel,
	}, nil
}

func (r *MMSRouter) StartRouter(ctx context.Context, wg *sync.WaitGroup, certPath *string, certKeyPath *string) {
	log.Println("Starting MMS Router")
	wg.Add(4)
	go func() {
		log.Println("Websocket listening on:", r.httpServer.Addr)
		if *certPath != "" && *certKeyPath != "" {
			if err := r.httpServer.ListenAndServeTLS(*certPath, *certKeyPath); err != nil {
				log.Println(err)
			}
		} else {
			if err := r.httpServer.ListenAndServe(); err != nil {
				log.Println(err)
			}
		}
		wg.Done()
	}()
	go handleIncomingMessages(ctx, r, wg)
	go handleOutgoingMessages(ctx, r, wg)
	go r.messageGC(ctx, wg)
	<-ctx.Done()
	log.Println("Shutting down MMS router")
	close(r.incomingChannel)
	close(r.outgoingChannel)
	if err := r.httpServer.Shutdown(context.Background()); err != nil {
		log.Println(err)
	}
	wg.Done()
}

// Function for garbage collection of expired messages
func (r *MMSRouter) messageGC(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute): // run every 5 minutes
			r.erMu.RLock()
			now := time.Now().UnixMilli()
			for _, er := range r.edgeRouters {
				er.MsgMu.Lock()
				for _, m := range er.Messages {
					expires := m.GetProtocolMessage().GetSendMessage().GetApplicationMessage().GetHeader().GetExpires()
					if now > expires {
						delete(er.Messages, m.Uuid)
					}
				}
				er.MsgMu.Unlock()
			}
			r.erMu.RUnlock()
		}
	}
}

func handleHttpConnection(p2p *host.Host, pubSub *pubsub.PubSub, incomingChannel chan *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, subs map[string]*Subscription, subMu *sync.RWMutex, edgeRouters map[string]*EdgeRouter, erMu *sync.RWMutex, topicHandles map[string]*pubsub.Topic, ctx context.Context, wg *sync.WaitGroup) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		wg.Add(1)
		c, err := websocket.Accept(writer, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			log.Println("Could not establish websocket connection", err)
			wg.Done()
			return
		}
		defer func(c *websocket.Conn, code websocket.StatusCode, reason string) {
			err := c.Close(code, reason)
			if err != nil {
				log.Println("Could not close connection:", err)
			}
			wg.Done()
		}(c, websocket.StatusInternalError, "PANIC!!!")

		// Set the read limit to 1 MiB instead of 32 KiB
		c.SetReadLimit(WsReadLimit)

		mmtpMessage, _, err := rw.ReadMessage(ctx, c)
		if err != nil {
			log.Println("Could not read message:", err)
			if err = c.Close(websocket.StatusUnsupportedData, "The first message could not be parsed as an MMTP message"); err != nil {
				log.Println(err)
			}
			return
		}

		protoMessage := mmtpMessage.GetProtocolMessage()
		if mmtpMessage.MsgType != mmtp.MsgType_PROTOCOL_MESSAGE || protoMessage == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to be a Protocol Message containing a Connect message with the MRN of the Edge Router"); err != nil {
				log.Println(err)
			}
			return
		}

		connect := protoMessage.GetConnectMessage()
		if connect == nil {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to contain a Connect message with the MRN of the Edge Router"); err != nil {
				log.Println(err)
			}
			return
		}

		erMrn := connect.GetOwnMrn()
		if erMrn == "" {
			if err = c.Close(websocket.StatusUnsupportedData, "The first message needs to be a Connect message with the MRN of the Edge Router"); err != nil {
				log.Println(err)
			}
			return
		}
		erMrn = strings.ToLower(erMrn)

		//Make sure all authentication checks pass, and otherwise return. ER MUST be authenticated
		_, authenticated, err := auth.AuthenticateConsumer(request, erMrn)

		if err != nil {
			var certErr *auth.CertValErr
			var sigAlgErr *auth.SigAlgErr
			var mrnErr *auth.MrnMismatchErr
			switch {
			case errors.As(err, &certErr):
			case errors.As(err, &sigAlgErr):
				if wsErr := c.Close(websocket.StatusPolicyViolation, err.Error()); wsErr != nil {
					log.Println(wsErr)
				}
			case errors.As(err, &mrnErr):
				if wsErr := c.Close(websocket.StatusUnsupportedData, err.Error()); wsErr != nil {
					log.Println(wsErr)
				}
			default:
				log.Printf("Unknown error occured: %s\n", err.Error())
			}
			return
		} else if !authenticated {
			if wsErr := c.Close(websocket.StatusPolicyViolation, "Could not authenticate Edge Router"); wsErr != nil {
				log.Println(wsErr)
			}
			return
		}

		var e *EdgeRouter
		if connect.ReconnectToken != nil {
			erMu.RLock()
			e = edgeRouters[erMrn]
			erMu.RUnlock()
			if e == nil {
				errorMsg := "No existing session was found for the given MRN"
				resp := &mmtp.MmtpMessage{
					MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
					Uuid:    uuid.NewString(),
					Body: &mmtp.MmtpMessage_ResponseMessage{
						ResponseMessage: &mmtp.ResponseMessage{
							ResponseToUuid: mmtpMessage.GetUuid(),
							Response:       mmtp.ResponseEnum_ERROR,
							ReasonText:     &errorMsg,
						}},
				}
				err = rw.WriteMessage(request.Context(), c, resp)
				if err != nil {
					log.Println("Could not send response message:", err)
					return
				}
			}
			if connect.GetReconnectToken() != e.ReconnectToken {
				errorMsg := "The given reconnect token does not match the one that is stored"
				resp := &mmtp.MmtpMessage{
					MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
					Uuid:    uuid.NewString(),
					Body: &mmtp.MmtpMessage_ResponseMessage{
						ResponseMessage: &mmtp.ResponseMessage{
							ResponseToUuid: mmtpMessage.GetUuid(),
							Response:       mmtp.ResponseEnum_ERROR,
							ReasonText:     &errorMsg,
						}},
				}
				err = rw.WriteMessage(request.Context(), c, resp)
				if err != nil {
					log.Println("Could not send response message:", err)
					return
				}
			}
		} else {
			e = &EdgeRouter{
				Consumer: consumer.Consumer{
					Mrn:           erMrn,
					Interests:     make([]string, 0, 1),
					Messages:      make(map[string]*mmtp.MmtpMessage),
					MsgMu:         &sync.RWMutex{},
					Notifications: make(map[string]*mmtp.MmtpMessage),
					NotifyMu:      &sync.RWMutex{},
				},
			}
		}

		e.ReconnectToken = uuid.NewString()

		erMu.Lock()
		edgeRouters[e.Mrn] = e
		erMu.Unlock()

		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					ReconnectToken: &e.ReconnectToken,
					Response:       mmtp.ResponseEnum_GOOD,
				}},
		}
		err = rw.WriteMessage(request.Context(), c, resp)
		if err != nil {
			log.Println("Could not send response message:", err)
			return
		}

		//Start thread that checks for incoming messages and notfies edgerouter
		wg.Add(1)
		erCtx, cancel := context.WithCancel(ctx)
		defer cancel() //When done handling edgerouter
		go e.CheckNewMessages(erCtx, c, wg)

		for {
			mmtpMessage, n, err := rw.ReadMessage(ctx, c)
			if err != nil {
				log.Println("Could not receive message:", err)
				reasonText := "Message could not be correctly parsed"
				resp = &mmtp.MmtpMessage{
					MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
					Uuid:    uuid.NewString(),
					Body: &mmtp.MmtpMessage_ResponseMessage{
						ResponseMessage: &mmtp.ResponseMessage{
							ResponseToUuid: mmtpMessage.GetUuid(),
							Response:       mmtp.ResponseEnum_ERROR,
							ReasonText:     &reasonText,
						},
					},
				}
				if err = rw.WriteMessage(request.Context(), c, resp); err != nil {
					return
				}
				if err = c.Close(websocket.StatusUnsupportedData, reasonText); err != nil {
					log.Println("Closing websocket failed after sending error response:", err)
				}
				return
			}

			switch mmtpMessage.GetMsgType() {
			case mmtp.MsgType_PROTOCOL_MESSAGE:
				{

					protoMessage = mmtpMessage.GetProtocolMessage()
					if protoMessage == nil {
						continue
					}
					switch protoMessage.ProtocolMsgType {
					case mmtp.ProtocolMessageType_SUBSCRIBE_MESSAGE:
						{
							err, errorText := handleSubscribe(mmtpMessage, subMu, subs, topicHandles, pubSub, e, wg, ctx, p2p, incomingChannel, request, c)
							if err != nil {
								log.Println("Failed handling Subscribe message:", err)
								errMsg.SendErrorMessage(mmtpMessage.GetUuid(), errorText, request.Context(), c)
							}
						}
					case mmtp.ProtocolMessageType_UNSUBSCRIBE_MESSAGE:
						{
							if err = handleUnsubscribe(mmtpMessage, subMu, subs, e, request, c); err != nil {
								log.Println("Failed handling Unsubscribe message:", err)
							}
						}
					case mmtp.ProtocolMessageType_SEND_MESSAGE:
						{
							if n > MessageSizeLimit {
								errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "The message size exceeds the allowed 50 KiB", request.Context(), c)
								break
							}
							handleSend(mmtpMessage, outgoingChannel, erMu, subMu, subs, e)
						}
					case mmtp.ProtocolMessageType_RECEIVE_MESSAGE:
						{
							if err = e.HandleReceive(mmtpMessage, request, c); err != nil {
								log.Println("Failed handling Receive message:", err)
							}
						}
					case mmtp.ProtocolMessageType_FETCH_MESSAGE:
						{
							if err = e.HandleFetch(mmtpMessage, request, c); err != nil {
								log.Println("Failed handling Fetch message:", err)
							}
						}
					case mmtp.ProtocolMessageType_DISCONNECT_MESSAGE:
						{
							if err = e.HandleDisconnect(mmtpMessage, request, c); err != nil {
								log.Println("Failed handling Disconnect message:", err)
							}
							return
						}
					case mmtp.ProtocolMessageType_CONNECT_MESSAGE:
						{
							errMsg.SendErrorMessage(mmtpMessage.GetUuid(), "Already connected", request.Context(), c)
						}
					default:
						continue
					}
				}
			case mmtp.MsgType_RESPONSE_MESSAGE:
			default:
				continue
			}
		}
	}
}

func handleSubscribe(mmtpMessage *mmtp.MmtpMessage, subMu *sync.RWMutex, subs map[string]*Subscription, topicHandles map[string]*pubsub.Topic, pubSub *pubsub.PubSub, e *EdgeRouter, wg *sync.WaitGroup, ctx context.Context, p2p *host.Host, incomingChannel chan *mmtp.MmtpMessage, request *http.Request, c *websocket.Conn) (error, string) {
	if subscribe := mmtpMessage.GetProtocolMessage().GetSubscribeMessage(); subscribe != nil {
		subject := subscribe.GetSubject()
		if subject == "" {
			return fmt.Errorf("cannot subscribe to empty subject"), "Cannot subscribe to empty subject"
		}
		subMu.Lock()
		sub, exists := subs[subject]
		if !exists {
			sub = NewSubscription(subject)
			topic, ok := topicHandles[subject]
			if !ok {
				t, err := pubSub.Join(subject)
				if err != nil {
					subMu.Unlock()
					return fmt.Errorf("was not able to join topic: %w", err), "Subscription failed"
				}
				topic = t
				topicHandles[subject] = topic
			}
			sub.AddSubscriber(e)
			sub.Topic = topic
			subscription, err := topic.Subscribe()
			if err != nil {
				subMu.Unlock()
				return fmt.Errorf("was not able to subscribe to topic: %w", err), "Subscription failed"
			}
			wg.Add(1)
			go handleSubscription(ctx, subscription, p2p, incomingChannel, wg)
			subs[subject] = sub
		} else {
			sub.AddSubscriber(e)
		}
		subMu.Unlock()
		e.Interests = append(e.Interests, subject)

		resp := &mmtp.MmtpMessage{
			MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
			Uuid:    uuid.NewString(),
			Body: &mmtp.MmtpMessage_ResponseMessage{
				ResponseMessage: &mmtp.ResponseMessage{
					ResponseToUuid: mmtpMessage.GetUuid(),
					Response:       mmtp.ResponseEnum_GOOD,
				}},
		}
		if err := rw.WriteMessage(request.Context(), c, resp); err != nil {
			log.Println("Could not send subscribe response to Edge Router:", err)
		}
	}
	return nil, ""
}

func handleUnsubscribe(mmtpMessage *mmtp.MmtpMessage, subMu *sync.RWMutex, subs map[string]*Subscription, e *EdgeRouter, request *http.Request, c *websocket.Conn) error {
	resp := &mmtp.MmtpMessage{
		MsgType: mmtp.MsgType_RESPONSE_MESSAGE,
		Uuid:    uuid.NewString(),
		Body: &mmtp.MmtpMessage_ResponseMessage{
			ResponseMessage: &mmtp.ResponseMessage{
				ResponseToUuid: mmtpMessage.GetUuid(),
				Response:       mmtp.ResponseEnum_GOOD,
			},
		},
	}
	if unsubscribe := mmtpMessage.GetProtocolMessage().GetUnsubscribeMessage(); unsubscribe != nil {

		subject := unsubscribe.GetSubject()
		if subject == "" {
			reasonText := "Tried to unsubscribe to empty subject"
			resp.GetResponseMessage().Response = mmtp.ResponseEnum_ERROR
			resp.GetResponseMessage().ReasonText = &reasonText
			err := rw.WriteMessage(request.Context(), c, resp)
			if err != nil {
				err = fmt.Errorf("could not write response to unsubscribe message: %w", err)
			}
			return err
		}
		subMu.Lock()
		sub, exists := subs[subject]
		if exists {
			sub.DeleteSubscriber(e)
		}
		subMu.Unlock()
		interests := e.Interests
		for i := range interests {
			if strings.EqualFold(subject, interests[i]) {
				interests[i] = interests[len(interests)-1]
				interests[len(interests)-1] = ""
				interests = interests[:len(interests)-1]
				e.Interests = interests
				break
			}
		}
	} else {
		resp.GetResponseMessage().Response = mmtp.ResponseEnum_ERROR
		reasonText := "Message does not contain a Unsubscribe message"
		resp.GetResponseMessage().ReasonText = &reasonText
	}

	err := rw.WriteMessage(request.Context(), c, resp)
	if err != nil {
		err = fmt.Errorf("could not write response to unsubscribe message: %w", err)
	}

	return err
}

func handleSend(mmtpMessage *mmtp.MmtpMessage, outgoingChannel chan<- *mmtp.MmtpMessage, erMu *sync.RWMutex, subMu *sync.RWMutex, subs map[string]*Subscription, e *EdgeRouter) {
	if send := mmtpMessage.GetProtocolMessage().GetSendMessage(); send != nil {
		outgoingChannel <- mmtpMessage
		header := send.GetApplicationMessage().GetHeader()
		if len(header.GetRecipients().GetRecipients()) > 0 {
			erMu.RLock()
			for _, recipient := range header.GetRecipients().Recipients {
				subMu.RLock()
				sub, exists := subs[recipient]
				subMu.RUnlock()
				if exists {
					sub.subsMu.RLock()
					for _, er := range sub.Subscribers {
						if er.Mrn != e.Mrn { // Do not send the message back to where it came from
							if err := er.QueueMessage(mmtpMessage); err != nil {
								log.Println("Could not queue message to Edge Router:", err)
							}
						}
					}
					sub.subsMu.RUnlock()
				}
			}
			erMu.RUnlock()
		} else if header.GetSubject() != "" {
			subMu.RLock()
			sub, exists := subs[header.GetSubject()]
			if exists {
				for _, subscriber := range sub.Subscribers {
					if subscriber.Mrn != e.Mrn {
						if err := subscriber.QueueMessage(mmtpMessage); err != nil {
							log.Println("Could not queue message to Edge Router:", err)
						}
					}
				}
			}
			subMu.RUnlock()
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
			err := revocation.PerformOCSPCheck(clientCert, issuingCert, httpClient)
			if err != nil {
				return err
			}
		} else if len(clientCert.CRLDistributionPoints) > 0 {
			err := revocation.PerformCRLCheck(clientCert, httpClient, issuingCert)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("was not able to check revocation status of client certificate")
		}

		return nil
	}
}

func handleSubscription(ctx context.Context, sub *pubsub.Subscription, host *host.Host, incomingChannel chan<- *mmtp.MmtpMessage, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			sub.Cancel()
			return
		default:
			m, err := sub.Next(ctx)
			if err != nil {
				log.Println("Could not get message from subscription:", err)
				continue
			}
			if m.GetFrom() != (*host).ID() {
				var mmtpMessage mmtp.MmtpMessage
				if err = proto.Unmarshal(m.Data, &mmtpMessage); err != nil {
					log.Println("Could not unmarshal received message as an mmtp message:", err)
					continue
				}
				uid, err := uuid.Parse(mmtpMessage.GetUuid())
				if err != nil || uid.Version() != 4 {
					log.Println("The UUID of the message is not a valid version 4 UUID")
					continue
				}
				switch mmtpMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						if sendMsg := mmtpMessage.GetProtocolMessage().GetSendMessage(); sendMsg != nil {
							incomingChannel <- &mmtpMessage
						}
					}
				case mmtp.MsgType_RESPONSE_MESSAGE:
				default:
					continue
				}
			}
		}
	}
}

func handleIncomingMessages(ctx context.Context, router *MMSRouter, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for incomingMessage := range router.incomingChannel {
				switch incomingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						now := time.Now()
						nowMilli := now.UnixMilli()
						appMsg := incomingMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
						msgExpires := appMsg.GetHeader().GetExpires()
						if appMsg == nil || nowMilli > msgExpires || msgExpires > now.Add(ExpirationLimit).UnixMilli() {
							// message is nil, expired or has a too long expiration, so we discard it
							continue
						}
						switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
						case *mmtp.ApplicationMessageHeader_Subject:
							{
								router.subMu.RLock()
								for _, subscriber := range router.subscriptions[subjectOrRecipient.Subject].Subscribers {
									if err := subscriber.QueueMessage(incomingMessage); err != nil {
										log.Println("Could not queue message:", err)
										continue
									}
								}
								router.subMu.RUnlock()
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									router.subMu.RLock()
									sub := router.subscriptions[recipient]
									for _, er := range sub.Subscribers {
										err := er.QueueMessage(incomingMessage)
										if err != nil {
											log.Println("Could not queue message for Edge Router:", err)
										}
									}
									router.subMu.RUnlock()
								}
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

func handleOutgoingMessages(ctx context.Context, router *MMSRouter, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for outgoingMessage := range router.outgoingChannel {
				switch outgoingMessage.GetMsgType() {
				case mmtp.MsgType_PROTOCOL_MESSAGE:
					{
						appMsg := outgoingMessage.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
						if appMsg == nil {
							continue
						}
						msgBytes, err := proto.Marshal(outgoingMessage)
						if err != nil {
							log.Println("Could not marshal outgoing message:", err)
							continue
						}
						switch subjectOrRecipient := appMsg.GetHeader().GetSubjectOrRecipient().(type) {
						case *mmtp.ApplicationMessageHeader_Subject:
							{
								topic, ok := router.topicHandles[subjectOrRecipient.Subject]
								if !ok {
									topic, err = router.pubSub.Join(subjectOrRecipient.Subject)
									if err != nil {
										log.Println("Could not join topic:", err)
										continue
									}
									router.topicHandles[subjectOrRecipient.Subject] = topic
								}
								err = topic.Publish(ctx, msgBytes)
								if err != nil {
									log.Println("Could not publish message to topic:", err)
									continue
								}
							}
						case *mmtp.ApplicationMessageHeader_Recipients:
							{
								for _, recipient := range subjectOrRecipient.Recipients.GetRecipients() {
									topic, ok := router.topicHandles[recipient]
									if !ok {
										topic, err = router.pubSub.Join(recipient)
										if err != nil {
											log.Println("Could not join topic:", err)
											continue
										}
										router.topicHandles[recipient] = topic
									}
									err = topic.Publish(ctx, msgBytes)
									if err != nil {
										log.Println("Could not publish message to topic:", err)
										continue
									}
								}
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

func setupLibP2P(ctx context.Context, libp2pPort *int, privKeyFilePath *string) (host.Host, *drouting.RoutingDiscovery, error) {
	port := *libp2pPort
	var addrStrings []string
	if port != 0 {
		addrStrings = []string{
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
			fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port),
			fmt.Sprintf("/ip6/::/tcp/%d", port),
		}
	} else {
		addrStrings = []string{
			"/ip4/0.0.0.0/udp/0/quic-v1",
			"/ip4/0.0.0.0/tcp/0",
			"/ip6/::/udp/0/quic-v1",
			"/ip6/::/tcp/0",
		}
	}
	// TODO make the router discover its public IP address so it can be published

	beacons := make([]peerstore.AddrInfo, 0, 1)
	beaconsFile, err := os.Open("beacons.txt")
	if err == nil {
		fileScanner := bufio.NewScanner(beaconsFile)
		for fileScanner.Scan() {
			addrInfo, err := peerstore.AddrInfoFromString(fileScanner.Text())
			if err != nil {
				log.Println("Failed to parse beacon address:", err)
				continue
			}
			beacons = append(beacons, *addrInfo)
		}
	}

	identify.ActivationThresh = 3

	var node host.Host
	if *privKeyFilePath != "" {
		privKeyFile, err := os.ReadFile(*privKeyFilePath)
		if err != nil {
			return nil, nil, fmt.Errorf("could not open the provided private key file: %w", err)
		}
		keyData, _ := pem.Decode(privKeyFile)
		privKey, err := x509.ParseECPrivateKey(keyData.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse the provided private key file as an ECDSA key: %w", err)
		}

		privEc, _, err := crypto.ECDSAKeyPairFromKey(privKey)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse the ECDSA private key from the file: %w", err)
		}

		// start a libp2p node with the given private key
		node, err = libp2p.New(
			libp2p.ListenAddrStrings(addrStrings...),
			libp2p.Identity(privEc),
			libp2p.EnableNATService(),
			libp2p.EnableRelayService(),
			libp2p.EnableAutoRelayWithStaticRelays(beacons),
			libp2p.EnableHolePunching(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("could not create a libp2p node: %w", err)
		}
	} else {
		var err error
		// start a libp2p node with default settings
		node, err = libp2p.New(
			libp2p.ListenAddrStrings(addrStrings...),
			libp2p.EnableNATService(),
			libp2p.EnableRelayService(),
			libp2p.EnableAutoRelayWithStaticRelays(beacons),
			libp2p.EnableHolePunching(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("could not create a libp2p node: %w", err)
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
	log.Println("libp2p node addresses:", addrs)
	return node, rd, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	listeningPort := flag.Int("port", 8080, "The port number that this Router should listen on.")
	libp2pPort := flag.Int("libp2p-port", 0, "The port number that this Router should use to "+
		"open up to the Router Network. If not set, a random port is chosen.")
	privKeyFilePath := flag.String("privkey", "", "Path to a file containing a private key. If none is provided, a new private key will be generated every time the program is run.")
	certPath := flag.String("cert-path", "", "Path to a TLS certificate file. If none is provided, TLS will be disabled.")
	certKeyPath := flag.String("cert-key-path", "", "Path to a TLS certificate private key. If none is provided, TLS will be disabled.")
	clientCAs := flag.String("client-ca", "", "Path to a file containing a list of client CAs that can connect to this Router.")

	flag.Parse()

	node, rd, err := setupLibP2P(ctx, libp2pPort, privKeyFilePath)
	if err != nil {
		log.Println("Could not setup the libp2p backend:", err)
		return
	}

	pubSub, err := pubsub.NewGossipSub(ctx, node)
	if err != nil {
		panic(err)
	}

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	attempts := 0
	for !anyConnected && attempts < 10 {
		log.Println("Searching for peers...")
		peerChan, err := rd.FindPeers(ctx, "over here")
		if err != nil {
			panic(err)
		}
		for p := range peerChan {
			if p.ID == node.ID() {
				continue // No self connection
			}
			log.Println("Peer:", p)
			err := node.Connect(ctx, p)
			if err != nil {
				log.Println("Failed connecting to ", p.ID.String(), ", error:", err)
			} else {
				log.Println("Connected to:", p.ID.String())
				anyConnected = true
			}
		}
		attempts++
		time.Sleep(2 * time.Second)
	}
	log.Println("Peer discovery complete")

	incomingChannel := make(chan *mmtp.MmtpMessage, ChannelBufSize)
	outgoingChannel := make(chan *mmtp.MmtpMessage, ChannelBufSize)

	wg := &sync.WaitGroup{}

	router, err := NewMMSRouter(&node, pubSub, ":"+strconv.Itoa(*listeningPort), incomingChannel, outgoingChannel, ctx, wg, clientCAs)
	if err != nil {
		log.Println("Could not create MMS Router instance:", err)
		return
	}

	wg.Add(1)
	go router.StartRouter(ctx, wg, certPath, certKeyPath)

	// wait for a SIGINT or SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	log.Println("Received signal, shutting down...")

	cancel()
	wg.Wait()
	// shut the libp2p node down
	if err = node.Close(); err != nil {
		log.Println("libp2p node could not be shut down correctly")
	}
}
