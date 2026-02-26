package main

import (
	"fmt"
	"os"
	"sync"
)

// InboundMessage represents a message received from any channel.
type InboundMessage struct {
	Session string // target session name
	Content string // message text
	Channel string // source channel name
	UserID  string // user identifier on the channel
}

// OutboundMessage represents a message to send to a channel.
type OutboundMessage struct {
	Session string // source session name
	Content string // message text
	Role    string // "assistant", "system"
	Event   string // "message", "stop", "notification", "question"
}

// Channel defines the interface for a messaging channel.
type Channel interface {
	Name() string
	Send(session string, msg OutboundMessage) error
	Start(handler func(msg InboundMessage)) error
	Stop() error
}

// Router orchestrates all channels, routing inbound messages to tmux
// and broadcasting outbound messages (from hooks) to all channels.
type Router struct {
	mu       sync.RWMutex
	channels []Channel
	config   *Config
	handler  func(msg InboundMessage) // processes inbound messages
}

// NewRouter creates a router with the given config.
func NewRouter(config *Config) *Router {
	return &Router{
		config:   config,
		channels: make([]Channel, 0),
	}
}

// AddChannel registers a channel with the router.
func (r *Router) AddChannel(ch Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = append(r.channels, ch)
}

// Start launches all registered channels.
func (r *Router) Start(handler func(msg InboundMessage)) error {
	r.handler = handler

	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, ch := range r.channels {
		chName := ch.Name()
		go func(c Channel) {
			fmt.Fprintf(os.Stderr, "router: starting channel %s\n", chName)
			if err := c.Start(handler); err != nil {
				fmt.Fprintf(os.Stderr, "router: channel %s error: %v\n", chName, err)
			}
		}(ch)
	}

	return nil
}

// Broadcast sends an outbound message to all channels.
func (r *Router) Broadcast(msg OutboundMessage) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, ch := range r.channels {
		go func(c Channel) {
			defer func() { recover() }()
			if err := c.Send(msg.Session, msg); err != nil {
				fmt.Fprintf(os.Stderr, "router: send to %s failed: %v\n", c.Name(), err)
			}
		}(ch)
	}
}

// Stop shuts down all channels.
func (r *Router) Stop() {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, ch := range r.channels {
		ch.Stop()
	}
}

// ChannelNames returns the names of all registered channels.
func (r *Router) ChannelNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, len(r.channels))
	for i, ch := range r.channels {
		names[i] = ch.Name()
	}
	return names
}
