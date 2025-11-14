package bot

import (
	"fmt"
	"log"
	"sync"

	"github.com/bwmarrin/discordgo"
)

type (
	Bot struct {
		session          *discordgo.Session
		subscriptions    map[string][]subscription // key: voiceChannelID
		mu               sync.RWMutex
		registeredCmdIDs map[string][]*discordgo.ApplicationCommand // guildID -> commands
	}

	subscription struct {
		voiceChannelId string
		textChannelId  string
		guildId        string
	}
)

func NewBot(token string) (*Bot, error) {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

	bot := &Bot{
		session:          dg,
		subscriptions:    make(map[string][]subscription),
		registeredCmdIDs: make(map[string][]*discordgo.ApplicationCommand),
	}

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
		}
	case discordgo.InteractionMessageComponent:
		data := i.MessageComponentData()

		switch data.CustomID {
		case "subscribe_channel_select":
			b.handleChannelSelect(s, i)
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
	var message string
	var channelID string

	if vsu.BeforeUpdate == nil {
		// User joined a voice channel (no previous state)
		if vsu.ChannelID != "" {
			channelID = vsu.ChannelID
			channel, err := s.Channel(channelID)
			channelName := channelID
			if err == nil {
				channelName = channel.Name
			}
			message = fmt.Sprintf("🔊 **%s** joined **%s**", username, channelName)
		}
	} else {
		// User was already in a voice channel
		oldChannelID := vsu.BeforeUpdate.ChannelID
		newChannelID := vsu.ChannelID

		if oldChannelID != "" && newChannelID == "" {
			// User left voice channel
			channelID = oldChannelID
			channel, err := s.Channel(channelID)
			channelName := channelID
			if err == nil {
				channelName = channel.Name
			}
			message = fmt.Sprintf("🔇 **%s** left **%s**", username, channelName)
		} else if oldChannelID != newChannelID && newChannelID != "" {
			// User moved to a different channel
			channelID = newChannelID
			channel, err := s.Channel(channelID)
			channelName := channelID
			if err == nil {
				channelName = channel.Name
			}
			message = fmt.Sprintf("🔊 **%s** joined **%s**", username, channelName)

			// Also notify the old channel
			if oldChannelID != "" {
				oldChannel, err := s.Channel(oldChannelID)
				oldChannelName := oldChannelID
				if err == nil {
					oldChannelName = oldChannel.Name
				}
				oldMessage := fmt.Sprintf("🔇 **%s** left **%s**", username, oldChannelName)
				b.sendNotifications(s, oldChannelID, oldMessage)
			}
		}
	}

	if message != "" && channelID != "" {
		b.sendNotifications(s, channelID, message)
	}
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
