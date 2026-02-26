package main

import (
	"fmt"
	"os"
)

// DiscordChannel implements Channel for Discord (stub).
// Full implementation requires discordgo library and a bot token.
type DiscordChannel struct {
	config  *Config
	token   string
	handler func(msg InboundMessage)
	cancel  chan struct{}
}

func NewDiscordChannel(config *Config) *DiscordChannel {
	return &DiscordChannel{
		config: config,
		token:  os.Getenv("DISCORD_BOT_TOKEN"),
		cancel: make(chan struct{}),
	}
}

func (d *DiscordChannel) Name() string { return "discord" }

func (d *DiscordChannel) Send(session string, msg OutboundMessage) error {
	// Stub: Discord sending not yet implemented.
	// Will require discordgo library and channel ID mapping.
	return nil
}

func (d *DiscordChannel) Start(handler func(msg InboundMessage)) error {
	d.handler = handler

	if d.token == "" {
		fmt.Fprintf(os.Stderr, "discord: DISCORD_BOT_TOKEN not set, channel disabled\n")
		return nil
	}

	// Stub: Discord bot connection not yet implemented.
	// Full implementation would:
	// 1. Connect to Discord gateway via discordgo
	// 2. Map Discord channels to CCC sessions (via config)
	// 3. Relay messages bidirectionally
	fmt.Fprintf(os.Stderr, "discord: channel stub loaded (implementation pending)\n")
	return nil
}

func (d *DiscordChannel) Stop() error {
	close(d.cancel)
	return nil
}
