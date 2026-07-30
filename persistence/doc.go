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

// Package persistence provides the state-store contract shared by MMS
// Consumers, Routers, and Edge Routers.
//
// SQLiteStore provides restart-safe local persistence. MemoryStore implements
// the same behavior for ephemeral deployments and tests. Runtime components
// depend only on Store, so backend selection happens once during application
// startup.
//
// A message is stored once and linked to each destination session by a
// delivery. Notifications and successful delivery are tracked independently:
// MarkNotified suppresses repeated notifications, while DeleteDeliveries
// acknowledges that message content was successfully written to a consumer.
//
// Store implementations provide at-least-once behavior around network writes.
// Callers should use MMTP message UUIDs to make duplicate delivery idempotent.
package persistence
