package bot

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type (
	Bot struct {
		session          *discordgo.Session
		subscriptions    map[string][]subscription // key: voiceChannelID
		mu               sync.RWMutex
		registeredCmdIDs map[string][]*discordgo.ApplicationCommand // guildID -> commands
		debounceInterval time.Duration
		debouncers       map[string]*debouncer // key: userID:channelID
		debounceMu       sync.RWMutex
	}

	subscription struct {
		voiceChannelId string
		textChannelId  string
		guildId        string
	}

	debouncer struct {
		timer   *time.Timer
		message string
		mu      sync.Mutex
	}
)

func NewBot(token string) (*Bot, error) {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

	// Get debounce interval from environment or use default
	debounceInterval := 3 * time.Second // Default 3 seconds
	if envInterval := os.Getenv("DEBOUNCE_INTERVAL"); envInterval != "" {
		if duration, err := time.ParseDuration(envInterval); err == nil {
			debounceInterval = duration
		} else {
			log.Printf("Invalid DEBOUNCE_INTERVAL value '%s', using default 3s", envInterval)
		}
	}

	bot := &Bot{
		session:          dg,
		subscriptions:    make(map[string][]subscription),
		registeredCmdIDs: make(map[string][]*discordgo.ApplicationCommand),
		debounceInterval: debounceInterval,
		debouncers:       make(map[string]*debouncer),
	}

	log.Printf("Debounce interval set to: %v", debounceInterval)

	// Ready handler registers commands in the bot's guilds
	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
		for _, guild := range r.Guilds {
			bot.registerCommands(s, guild.ID)
		}
	})

	dg.AddHandler(func(s *discordgo.Session, vsu *discordgo.VoiceStateUpdate) {
		bot.voiceStateUpdate(s, vsu)
	})

	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		bot.interactionCreate(s, i)
	})

	return bot, nil
}

func (b *Bot) Start() error {
	return b.session.Open()
}

func (b *Bot) Stop() {
	// Unregister all commands from all guilds
	for guildID, commands := range b.registeredCmdIDs {
		for _, cmd := range commands {
			err := b.session.ApplicationCommandDelete(b.session.State.User.ID, guildID, cmd.ID)
			if err != nil {
				log.Printf("Failed to delete command %v in guild %v: %v", cmd.Name, guildID, err)
			}
		}
	}

	b.session.Close()
}

func (b *Bot) registerCommands(s *discordgo.Session, guildID string) {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "subscribe",
			Description: "Subscribe to voice channel notifications",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionChannel,
					Name:        "voice-channel",
					Description: "The voice channel to monitor",
					Required:    false,
					ChannelTypes: []discordgo.ChannelType{
						discordgo.ChannelTypeGuildVoice,
					},
				},
			},
		},
		{
			Name:        "unsubscribe",
			Description: "Unsubscribe from voice channel notifications",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionChannel,
					Name:        "voice-channel",
					Description: "The voice channel to stop monitoring",
					Required:    false,
					ChannelTypes: []discordgo.ChannelType{
						discordgo.ChannelTypeGuildVoice,
					},
				},
			},
		},
	}

	for _, cmd := range commands {
		registeredCmd, err := s.ApplicationCommandCreate(s.State.User.ID, guildID, cmd)
		if err != nil {
			log.Printf("Cannot create '%v' command in guild %v: %v", cmd.Name, guildID, err)
		} else {
			// Store registered command IDs for cleanup
			b.mu.Lock()
			b.registeredCmdIDs[guildID] = append(b.registeredCmdIDs[guildID], registeredCmd)
			b.mu.Unlock()
		}
	}
}

func (b *Bot) interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()

		switch data.Name {
		case "subscribe":
			b.handleSubscribe(s, i)
		case "unsubscribe":
			b.handleUnsubscribe(s, i)
		}
	case discordgo.InteractionMessageComponent:
		data := i.MessageComponentData()

		switch data.CustomID {
		case "subscribe_channel_select":
			b.handleChannelSelect(s, i)
		case "unsubscribe_channel_select":
			b.handleUnsubscribeChannelSelect(s, i)
		}
	}
}

func (b *Bot) handleSubscribe(s *discordgo.Session, i *discordgo.InteractionCreate) {
	options := i.ApplicationCommandData().Options

	// Get the text channel where the command was issued
	textChannelID := i.ChannelID
	guildID := i.GuildID

	// Check if a voice channel was provided
	if len(options) == 0 {
		// No voice channel provided - show selection dialog
		b.handleSubscribeWithDialog(s, i)
		return
	}

	// Voice channel was provided
	voiceChannelID := options[0].ChannelValue(s).ID

	// Add subscription
	b.mu.Lock()
	if b.subscriptions[voiceChannelID] == nil {
		b.subscriptions[voiceChannelID] = []subscription{}
	}

	// Check if already subscribed
	alreadySubscribed := false
	for _, sub := range b.subscriptions[voiceChannelID] {
		if sub.textChannelId == textChannelID && sub.voiceChannelId == voiceChannelID {
			alreadySubscribed = true
			break
		}
	}

	if !alreadySubscribed {
		b.subscriptions[voiceChannelID] = append(b.subscriptions[voiceChannelID], subscription{
			voiceChannelId: voiceChannelID,
			textChannelId:  textChannelID,
			guildId:        guildID,
		})
	}
	b.mu.Unlock()

	// Get voice channel name
	channel, err := s.Channel(voiceChannelID)
	channelName := voiceChannelID
	if err == nil {
		channelName = channel.Name
	}

	responseText := fmt.Sprintf("✅ Subscribed! This channel will receive notifications for voice activity in **%s**", channelName)
	if alreadySubscribed {
		responseText = fmt.Sprintf("ℹ️ Already subscribed to **%s**", channelName)
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: responseText,
		},
	})
}

func (b *Bot) handleSubscribeWithDialog(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guildID := i.GuildID

	// Get all voice channels in the guild
	channels, err := s.GuildChannels(guildID)
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ Error fetching channels",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Filter voice channels and create select menu options
	var options []discordgo.SelectMenuOption
	for _, channel := range channels {
		if channel.Type == discordgo.ChannelTypeGuildVoice {
			options = append(options, discordgo.SelectMenuOption{
				Label: channel.Name,
				Value: channel.ID,
			})
		}
	}

	if len(options) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ No voice channels found in this server",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Respond with a select menu
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Select a voice channel to monitor:",
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    "subscribe_channel_select",
							Placeholder: "Choose a voice channel",
							Options:     options,
						},
					},
				},
			},
		},
	})
}

func (b *Bot) handleChannelSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()

	// Get the selected voice channel ID
	if len(data.Values) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ No channel selected",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	voiceChannelID := data.Values[0]
	textChannelID := i.ChannelID
	guildID := i.GuildID

	// Add subscription
	b.mu.Lock()
	if b.subscriptions[voiceChannelID] == nil {
		b.subscriptions[voiceChannelID] = []subscription{}
	}

	// Check if already subscribed
	alreadySubscribed := false
	for _, sub := range b.subscriptions[voiceChannelID] {
		if sub.textChannelId == textChannelID && sub.voiceChannelId == voiceChannelID {
			alreadySubscribed = true
			break
		}
	}

	if !alreadySubscribed {
		b.subscriptions[voiceChannelID] = append(b.subscriptions[voiceChannelID], subscription{
			voiceChannelId: voiceChannelID,
			textChannelId:  textChannelID,
			guildId:        guildID,
		})
	}
	b.mu.Unlock()

	// Get voice channel name
	channel, err := s.Channel(voiceChannelID)
	channelName := voiceChannelID
	if err == nil {
		channelName = channel.Name
	}

	responseText := fmt.Sprintf("✅ Subscribed! This channel will receive notifications for voice activity in **%s**", channelName)
	if alreadySubscribed {
		responseText = fmt.Sprintf("ℹ️ Already subscribed to **%s**", channelName)
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    responseText,
			Components: []discordgo.MessageComponent{}, // Remove the select menu
		},
	})
}

func (b *Bot) handleUnsubscribe(s *discordgo.Session, i *discordgo.InteractionCreate) {
	options := i.ApplicationCommandData().Options
	textChannelID := i.ChannelID
	guildID := i.GuildID

	// Check if a voice channel was provided
	if len(options) == 0 {
		// No voice channel provided - need to determine behavior
		b.handleUnsubscribeWithoutChannel(s, i, textChannelID, guildID)
		return
	}

	// Voice channel was provided
	voiceChannelID := options[0].ChannelValue(s).ID

	// Remove subscription
	b.mu.Lock()
	removed := false
	if subs, exists := b.subscriptions[voiceChannelID]; exists {
		for idx, sub := range subs {
			if sub.textChannelId == textChannelID && sub.voiceChannelId == voiceChannelID {
				// Remove this subscription
				b.subscriptions[voiceChannelID] = append(subs[:idx], subs[idx+1:]...)
				removed = true
				break
			}
		}
		// Clean up empty subscription lists
		if len(b.subscriptions[voiceChannelID]) == 0 {
			delete(b.subscriptions, voiceChannelID)
		}
	}
	b.mu.Unlock()

	// Get voice channel name
	channel, err := s.Channel(voiceChannelID)
	channelName := voiceChannelID
	if err == nil {
		channelName = channel.Name
	}

	responseText := fmt.Sprintf("✅ Unsubscribed from **%s**", channelName)
	if !removed {
		responseText = fmt.Sprintf("ℹ️ Not subscribed to **%s**", channelName)
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: responseText,
		},
	})
}

func (b *Bot) handleUnsubscribeWithoutChannel(s *discordgo.Session, i *discordgo.InteractionCreate, textChannelID, guildID string) {
	// Find all subscriptions for this text channel
	b.mu.RLock()
	var matchingVoiceChannels []string
	for voiceChannelID, subs := range b.subscriptions {
		for _, sub := range subs {
			if sub.textChannelId == textChannelID && sub.guildId == guildID {
				matchingVoiceChannels = append(matchingVoiceChannels, voiceChannelID)
				break
			}
		}
	}
	b.mu.RUnlock()

	if len(matchingVoiceChannels) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "ℹ️ No active subscriptions in this channel",
			},
		})
		return
	}

	if len(matchingVoiceChannels) == 1 {
		// Single subscription - unsubscribe automatically
		voiceChannelID := matchingVoiceChannels[0]

		b.mu.Lock()
		if subs, exists := b.subscriptions[voiceChannelID]; exists {
			for idx, sub := range subs {
				if sub.textChannelId == textChannelID {
					b.subscriptions[voiceChannelID] = append(subs[:idx], subs[idx+1:]...)
					break
				}
			}
			if len(b.subscriptions[voiceChannelID]) == 0 {
				delete(b.subscriptions, voiceChannelID)
			}
		}
		b.mu.Unlock()

		// Get voice channel name
		channel, err := s.Channel(voiceChannelID)
		channelName := voiceChannelID
		if err == nil {
			channelName = channel.Name
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("✅ Unsubscribed from **%s**", channelName),
			},
		})
		return
	}

	// Multiple subscriptions - show selection dialog
	b.handleUnsubscribeWithDialog(s, i, matchingVoiceChannels)
}

func (b *Bot) handleUnsubscribeWithDialog(s *discordgo.Session, i *discordgo.InteractionCreate, voiceChannelIDs []string) {
	// Create select menu options from voice channel IDs
	var options []discordgo.SelectMenuOption
	for _, channelID := range voiceChannelIDs {
		channel, err := s.Channel(channelID)
		channelName := channelID
		if err == nil {
			channelName = channel.Name
		}
		options = append(options, discordgo.SelectMenuOption{
			Label: channelName,
			Value: channelID,
		})
	}

	// Respond with a select menu
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Select a voice channel to unsubscribe from:",
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    "unsubscribe_channel_select",
							Placeholder: "Choose a voice channel",
							Options:     options,
						},
					},
				},
			},
		},
	})
}

func (b *Bot) handleUnsubscribeChannelSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()

	// Get the selected voice channel ID
	if len(data.Values) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ No channel selected",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	voiceChannelID := data.Values[0]
	textChannelID := i.ChannelID

	// Remove subscription
	b.mu.Lock()
	removed := false
	if subs, exists := b.subscriptions[voiceChannelID]; exists {
		for idx, sub := range subs {
			if sub.textChannelId == textChannelID && sub.voiceChannelId == voiceChannelID {
				b.subscriptions[voiceChannelID] = append(subs[:idx], subs[idx+1:]...)
				removed = true
				break
			}
		}
		if len(b.subscriptions[voiceChannelID]) == 0 {
			delete(b.subscriptions, voiceChannelID)
		}
	}
	b.mu.Unlock()

	// Get voice channel name
	channel, err := s.Channel(voiceChannelID)
	channelName := voiceChannelID
	if err == nil {
		channelName = channel.Name
	}

	responseText := fmt.Sprintf("✅ Unsubscribed from **%s**", channelName)
	if !removed {
		responseText = fmt.Sprintf("ℹ️ Not subscribed to **%s**", channelName)
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    responseText,
			Components: []discordgo.MessageComponent{}, // Remove the select menu
		},
	})
}

func (b *Bot) voiceStateUpdate(s *discordgo.Session, vsu *discordgo.VoiceStateUpdate) {
	// Get the member info
	member := vsu.Member
	if member == nil {
		// Try to get member info
		var err error
		member, err = s.GuildMember(vsu.GuildID, vsu.UserID)
		if err != nil {
			log.Printf("Error getting member info: %v", err)
			return
		}
	}

	// Ignore bot users
	if member.User.Bot {
		return
	}

	username := member.User.Username
	if member.Nick != "" {
		username = member.Nick
	}

	// Check if user joined or left a voice channel
	if vsu.BeforeUpdate == nil {
		// User joined a voice channel (no previous state)
		if vsu.ChannelID != "" {
			channelID := vsu.ChannelID
			channel, err := s.Channel(channelID)
			channelName := channelID
			if err == nil {
				channelName = channel.Name
			}
			message := fmt.Sprintf("🔊 **%s** joined **%s**", username, channelName)
			b.debounceNotification(s, vsu.UserID, channelID, message)
		}
	} else {
		// User was already in a voice channel
		oldChannelID := vsu.BeforeUpdate.ChannelID
		newChannelID := vsu.ChannelID

		if oldChannelID != "" && newChannelID == "" {
			// User left voice channel
			channelID := oldChannelID
			channel, err := s.Channel(channelID)
			channelName := channelID
			if err == nil {
				channelName = channel.Name
			}
			message := fmt.Sprintf("🔇 **%s** left **%s**", username, channelName)
			b.debounceNotification(s, vsu.UserID, channelID, message)
		} else if oldChannelID != newChannelID && newChannelID != "" {
			// User moved to a different channel
			// Notify old channel about leaving
			if oldChannelID != "" {
				oldChannel, err := s.Channel(oldChannelID)
				oldChannelName := oldChannelID
				if err == nil {
					oldChannelName = oldChannel.Name
				}
				oldMessage := fmt.Sprintf("🔇 **%s** left **%s**", username, oldChannelName)
				b.debounceNotification(s, vsu.UserID, oldChannelID, oldMessage)
			}

			// Notify new channel about joining
			channel, err := s.Channel(newChannelID)
			channelName := newChannelID
			if err == nil {
				channelName = channel.Name
			}
			message := fmt.Sprintf("🔊 **%s** joined **%s**", username, channelName)
			b.debounceNotification(s, vsu.UserID, newChannelID, message)
		}
	}
}

func (b *Bot) debounceNotification(s *discordgo.Session, userID, channelID, message string) {
	key := fmt.Sprintf("%s:%s", userID, channelID)

	b.debounceMu.Lock()
	deb, exists := b.debouncers[key]
	if !exists {
		deb = &debouncer{}
		b.debouncers[key] = deb
	}
	b.debounceMu.Unlock()

	deb.mu.Lock()
	defer deb.mu.Unlock()

	// Update the message
	deb.message = message

	// If there's an existing timer, stop it
	if deb.timer != nil {
		deb.timer.Stop()
	}

	// Create a new timer
	deb.timer = time.AfterFunc(b.debounceInterval, func() {
		deb.mu.Lock()
		finalMessage := deb.message
		deb.mu.Unlock()

		// Send the notification
		b.sendNotifications(s, channelID, finalMessage)

		// Clean up the debouncer
		b.debounceMu.Lock()
		delete(b.debouncers, key)
		b.debounceMu.Unlock()
	})
}

func (b *Bot) sendNotifications(s *discordgo.Session, voiceChannelID string, message string) {
	b.mu.RLock()
	subscriptions := b.subscriptions[voiceChannelID]
	b.mu.RUnlock()

	for _, sub := range subscriptions {
		_, err := s.ChannelMessageSend(sub.textChannelId, message)
		if err != nil {
			log.Printf("Error sending notification to channel %v: %v", sub.textChannelId, err)
		}
	}
}
