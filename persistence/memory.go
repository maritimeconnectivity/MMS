/*
 * Copyright 2026 Maritime Connectivity Platform Consortium
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

package persistence

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maritimeconnectivity/MMS/mmtp"
	"google.golang.org/protobuf/proto"
)

type memorySession struct {
	session   Session
	tokenHash []byte
}

type memoryMessage struct {
	message   *mmtp.MmtpMessage
	createdAt time.Time
	expiresAt time.Time
}

type memoryDelivery struct {
	notifiedAt *time.Time
}

type memoryOutboxMessage struct {
	message   *mmtp.MmtpMessage
	createdAt time.Time
}

// MemoryStore implements Store with process-local state.
//
// It is safe for concurrent use. All state is discarded when the process
// exits, and Close is therefore a no-op. Protobuf messages are cloned when
// entering and leaving the store so callers cannot mutate queued state.
type MemoryStore struct {
	mu            sync.RWMutex
	sessions      map[string]memorySession
	subscriptions map[string]map[string]struct{}
	messages      map[string]memoryMessage
	deliveries    map[string]map[string]memoryDelivery
	outbox        map[string]memoryOutboxMessage
	settings      map[string]string
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty, concurrency-safe in-memory state store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions:      make(map[string]memorySession),
		subscriptions: make(map[string]map[string]struct{}),
		messages:      make(map[string]memoryMessage),
		deliveries:    make(map[string]map[string]memoryDelivery),
		outbox:        make(map[string]memoryOutboxMessage),
		settings:      make(map[string]string),
	}
}

// Close implements Store. MemoryStore owns no external resources.
func (s *MemoryStore) Close() error {
	return nil
}

// UpsertSession creates or updates a reconnectable session.
func (s *MemoryStore) UpsertSession(_ context.Context, session Session, reconnectToken string) error {
	if session.ID == "" || reconnectToken == "" {
		return fmt.Errorf("session ID and reconnect token are required")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	if session.ExpiresAt.IsZero() {
		session.ExpiresAt = session.CreatedAt.Add(30 * 24 * time.Hour)
	}
	session.MRN = normalizeMRN(session.MRN)

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.sessions[session.ID]; ok {
		session.CreatedAt = existing.session.CreatedAt
	}
	s.sessions[session.ID] = memorySession{session: session, tokenHash: tokenHash(reconnectToken)}
	return nil
}

// RotateReconnectToken replaces a session's token and expiry.
func (s *MemoryStore) RotateReconnectToken(_ context.Context, id, reconnectToken string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	entry.tokenHash = tokenHash(reconnectToken)
	entry.session.ExpiresAt = expiresAt
	s.sessions[id] = entry
	return nil
}

// SessionByToken finds a non-expired session by its hashed reconnect token.
func (s *MemoryStore) SessionByToken(_ context.Context, kind, reconnectToken string) (*Session, error) {
	hash := tokenHash(reconnectToken)
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.sessions {
		if entry.session.Kind == kind && !entry.session.ExpiresAt.Before(now) && bytes.Equal(entry.tokenHash, hash) {
			session := entry.session
			return &session, nil
		}
	}
	return nil, ErrNotFound
}

// SessionByID finds a session by its stable identifier.
func (s *MemoryStore) SessionByID(_ context.Context, id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	session := entry.session
	return &session, nil
}

// Sessions returns non-expired sessions of kind, ordered by session ID.
func (s *MemoryStore) Sessions(_ context.Context, kind string) ([]Session, error) {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Session, 0, len(s.sessions))
	for _, entry := range s.sessions {
		if entry.session.Kind == kind && !entry.session.ExpiresAt.Before(now) {
			result = append(result, entry.session)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// DeleteSession removes a session and its subscriptions and deliveries.
func (s *MemoryStore) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	delete(s.subscriptions, id)
	delete(s.deliveries, id)
	return nil
}

// SetDirectMessages updates a session's direct-message subscription flag.
func (s *MemoryStore) SetDirectMessages(_ context.Context, id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	entry.session.DirectMessages = enabled
	s.sessions[id] = entry
	return nil
}

// Subscribe associates subject with an existing session.
func (s *MemoryStore) Subscribe(_ context.Context, id, subject string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ErrNotFound
	}
	if s.subscriptions[id] == nil {
		s.subscriptions[id] = make(map[string]struct{})
	}
	s.subscriptions[id][subject] = struct{}{}
	return nil
}

// Unsubscribe removes a subject association. Missing associations are ignored.
func (s *MemoryStore) Unsubscribe(_ context.Context, id, subject string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscriptions[id], subject)
	return nil
}

// Subscriptions returns a session's subjects in lexical order.
func (s *MemoryStore) Subscriptions(_ context.Context, id string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]string, 0, len(s.subscriptions[id]))
	for subject := range s.subscriptions[id] {
		result = append(result, subject)
	}
	sort.Strings(result)
	return result, nil
}

// QueueMessage atomically associates a cloned message with every supplied
// session. Reusing a UUID with different message content is rejected.
func (s *MemoryStore) QueueMessage(_ context.Context, sessionIDs []string, message *mmtp.MmtpMessage) error {
	if message == nil || message.GetUuid() == "" {
		return fmt.Errorf("message UUID is required")
	}
	expiresAt, err := messageExpiry(message)
	if err != nil {
		return err
	}
	cloned := proto.Clone(message).(*mmtp.MmtpMessage)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range sessionIDs {
		if _, ok := s.sessions[id]; !ok {
			return fmt.Errorf("insert delivery for session %q: %w", id, ErrNotFound)
		}
	}
	if existing, ok := s.messages[message.GetUuid()]; ok {
		if !proto.Equal(existing.message, message) {
			return fmt.Errorf("message UUID %q already exists with different content", message.GetUuid())
		}
	} else {
		s.messages[message.GetUuid()] = memoryMessage{
			message: cloned, createdAt: time.Now(), expiresAt: time.Unix(expiresAt, 0),
		}
	}
	for _, id := range sessionIDs {
		if s.deliveries[id] == nil {
			s.deliveries[id] = make(map[string]memoryDelivery)
		}
		if _, exists := s.deliveries[id][message.GetUuid()]; !exists {
			s.deliveries[id][message.GetUuid()] = memoryDelivery{}
		}
	}
	return nil
}

// PendingNotifications returns cloned, unexpired messages whose deliveries
// have not been marked as notified.
func (s *MemoryStore) PendingNotifications(_ context.Context, id string) ([]*mmtp.MmtpMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var result []memoryMessage
	for uuid, delivery := range s.deliveries[id] {
		message, ok := s.messages[uuid]
		if ok && delivery.notifiedAt == nil && !message.expiresAt.Before(now) {
			result = append(result, message)
		}
	}
	return cloneSortedMessages(result), nil
}

// MarkNotified records successful notification for the supplied deliveries.
func (s *MemoryStore) MarkNotified(_ context.Context, id string, uuids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, uuid := range uuids {
		if delivery, ok := s.deliveries[id][uuid]; ok {
			delivery.notifiedAt = &now
			s.deliveries[id][uuid] = delivery
		}
	}
	return nil
}

// FetchMessages returns cloned, unexpired messages for a session. Passing no
// UUIDs selects every queued message for that session.
func (s *MemoryStore) FetchMessages(_ context.Context, id string, uuids []string) ([]*mmtp.MmtpMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	filter := make(map[string]struct{}, len(uuids))
	for _, uuid := range uuids {
		filter[uuid] = struct{}{}
	}
	now := time.Now()
	var result []memoryMessage
	for uuid := range s.deliveries[id] {
		if len(filter) > 0 {
			if _, requested := filter[uuid]; !requested {
				continue
			}
		}
		message, ok := s.messages[uuid]
		if ok && !message.expiresAt.Before(now) {
			result = append(result, message)
		}
	}
	return cloneSortedMessages(result), nil
}

// DeleteDeliveries removes successfully transmitted deliveries.
func (s *MemoryStore) DeleteDeliveries(_ context.Context, id string, uuids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, uuid := range uuids {
		delete(s.deliveries[id], uuid)
	}
	return nil
}

// PurgeExpired removes expired messages, orphaned messages, and their delivery
// records.
func (s *MemoryStore) PurgeExpired(_ context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uuid, message := range s.messages {
		if message.expiresAt.Before(now) || !s.hasDelivery(uuid) {
			delete(s.messages, uuid)
			for id := range s.deliveries {
				delete(s.deliveries[id], uuid)
			}
		}
	}
	return nil
}

// PurgeExpiredSessions removes expired sessions and their dependent state.
func (s *MemoryStore) PurgeExpiredSessions(_ context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.sessions {
		if entry.session.ExpiresAt.Before(now) {
			delete(s.sessions, id)
			delete(s.subscriptions, id)
			delete(s.deliveries, id)
		}
	}
	return nil
}

// PutOutbox inserts or replaces a cloned outgoing message while retaining its
// original insertion order.
func (s *MemoryStore) PutOutbox(_ context.Context, message *mmtp.MmtpMessage) error {
	if message == nil || message.GetUuid() == "" {
		return fmt.Errorf("outbox message UUID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.outbox[message.GetUuid()]
	if !exists {
		entry.createdAt = time.Now()
	}
	entry.message = proto.Clone(message).(*mmtp.MmtpMessage)
	s.outbox[message.GetUuid()] = entry
	return nil
}

// DeleteOutbox removes an acknowledged outgoing message.
func (s *MemoryStore) DeleteOutbox(_ context.Context, uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.outbox, uuid)
	return nil
}

// Outbox returns cloned outgoing messages in insertion order.
func (s *MemoryStore) Outbox(_ context.Context) ([]*mmtp.MmtpMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := make([]memoryOutboxMessage, 0, len(s.outbox))
	for _, entry := range s.outbox {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].createdAt.Before(entries[j].createdAt) })
	result := make([]*mmtp.MmtpMessage, 0, len(entries))
	for _, entry := range entries {
		result = append(result, proto.Clone(entry.message).(*mmtp.MmtpMessage))
	}
	return result, nil
}

// SetSetting inserts or replaces a process-local string setting.
func (s *MemoryStore) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[key] = value
	return nil
}

// Setting returns a process-local setting or ErrNotFound.
func (s *MemoryStore) Setting(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.settings[key]
	if !ok {
		return "", ErrNotFound
	}
	return value, nil
}

// DeleteSetting removes a process-local setting.
func (s *MemoryStore) DeleteSetting(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.settings, key)
	return nil
}

func (s *MemoryStore) hasDelivery(uuid string) bool {
	for _, deliveries := range s.deliveries {
		if _, ok := deliveries[uuid]; ok {
			return true
		}
	}
	return false
}

func cloneSortedMessages(messages []memoryMessage) []*mmtp.MmtpMessage {
	sort.Slice(messages, func(i, j int) bool { return messages[i].createdAt.Before(messages[j].createdAt) })
	result := make([]*mmtp.MmtpMessage, 0, len(messages))
	for _, message := range messages {
		result = append(result, proto.Clone(message.message).(*mmtp.MmtpMessage))
	}
	return result
}

func normalizeMRN(mrn string) string {
	// Keep the same canonicalization used by SQLiteStore.
	return strings.ToLower(mrn)
}
