package bot

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type (
	Bot struct {
		session          *discordgo.Session
		subscriptions    map[string][]subscription // key: voiceChannelID
		mu               sync.RWMutex
		registeredCmdIds map[string][]*discordgo.ApplicationCommand // guildID -> commands
		debounceInterval time.Duration
		debouncers       map[string]*debouncer // key: userID:channelID
		debounceMu       sync.RWMutex
		persistence      *Persistence
		adminChannels    map[string]string // guildID -> channelID
	}

	subscription struct {
		VoiceChannelId string `json:"voice_channel_id"`
		TextChannelId  string `json:"text_channel_id"`
		GuildId        string `json:"guild_id"`
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

	// Get persistence file path from environment or use default
	persistenceFile := os.Getenv("PERSISTENCE_FILE")
	if persistenceFile == "" {
		persistenceFile = "subscriptions.json"
	}

	bot := &Bot{
		session:          dg,
		subscriptions:    make(map[string][]subscription),
		registeredCmdIds: make(map[string][]*discordgo.ApplicationCommand),
		debounceInterval: debounceInterval,
		debouncers:       make(map[string]*debouncer),
		persistence:      NewPersistence(persistenceFile),
		adminChannels:    make(map[string]string),
	}

	// Load persisted data
	if err := bot.loadPersistedData(); err != nil {
		log.Printf("Warning: Failed to load persisted data: %v", err)
	}

	// Load admin channels from environment variable
	bot.loadAdminChannelsFromEnv()

	// Ready handler registers commands in the bot's guilds
	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
		for _, guild := range r.Guilds {
			bot.registerCommands(s, guild.ID)
		}
	})

	// Voice state update handler (Notified when user joins, leaves, or moves voice channels)
	dg.AddHandler(func(s *discordgo.Session, vsu *discordgo.VoiceStateUpdate) {
		bot.voiceStateUpdate(s, vsu)
	})

	// Interaction create handler (Handles slash commands and component interactions)
	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		bot.interactionCreate(s, i)
	})

	return bot, nil
}

func (b *Bot) Start() error {
	return b.session.Open()
}

func (b *Bot) Stop() {
	// Save subscriptions before shutting down
	if err := b.savePersistedData(); err != nil {
		log.Printf("Error saving persisted data: %v", err)
	}

	// Unregister all commands from all guilds
	for guildId, commands := range b.registeredCmdIds {
		for _, cmd := range commands {
			err := b.session.ApplicationCommandDelete(b.session.State.User.ID, guildId, cmd.ID)
			if err != nil {
				log.Printf("Failed to delete command %v in guild %v: %v", cmd.Name, guildId, err)
			}
		}
	}

	b.session.Close()
}

func (b *Bot) registerCommands(s *discordgo.Session, guildId string) {
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
		{
			Name:        "list-subscriptions",
			Description: "List all voice channel subscriptions (admin channel only)",
		},
	}

	for _, cmd := range commands {
		registeredCmd, err := s.ApplicationCommandCreate(s.State.User.ID, guildId, cmd)
		if err != nil {
			log.Printf("Cannot create '%v' command in guild %v: %v", cmd.Name, guildId, err)
		} else {
			// Store registered command IDs for cleanup
			b.mu.Lock()
			b.registeredCmdIds[guildId] = append(b.registeredCmdIds[guildId], registeredCmd)
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
		case "list-subscriptions":
			b.handleListSubscriptions(s, i)
		}
	case discordgo.InteractionMessageComponent:
		data := i.MessageComponentData()

		if strings.HasPrefix(data.CustomID, "remove_sub:") {
			b.handleRemoveSubscriptionButton(s, i)
		} else {
			switch data.CustomID {
			case "subscribe_channel_select":
				b.handleChannelSelect(s, i)
			case "unsubscribe_channel_select":
				b.handleUnsubscribeChannelSelect(s, i)
			case "manage_subscription_select":
				b.handleManageSubscriptionSelect(s, i)
			case "back_to_subscription_list":
				b.handleBackToSubscriptionList(s, i)
			}
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
	alreadySubscribed := b.addSubscription(voiceChannelID, textChannelID, guildID)

	responseText := b.formatSubscribeResponse(s, voiceChannelID, alreadySubscribed)
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: responseText,
			Flags:   discordgo.MessageFlagsEphemeral,
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

	alreadySubscribed := b.addSubscription(voiceChannelID, textChannelID, guildID)
	responseText := b.formatSubscribeResponse(s, voiceChannelID, alreadySubscribed)

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
	removed := b.removeSubscription(voiceChannelID, textChannelID)
	responseText := b.formatUnsubscribeResponse(s, voiceChannelID, removed)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: responseText,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func (b *Bot) handleUnsubscribeWithoutChannel(s *discordgo.Session, i *discordgo.InteractionCreate, textChannelID, guildID string) {
	// Find all subscriptions for this text channel
	b.mu.RLock()
	var matchingVoiceChannels []string
	for voiceChannelID, subs := range b.subscriptions {
		for _, sub := range subs {
			if sub.TextChannelId == textChannelID && sub.GuildId == guildID {
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
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	if len(matchingVoiceChannels) == 1 {
		// Single subscription - unsubscribe automatically
		voiceChannelID := matchingVoiceChannels[0]
		b.removeSubscription(voiceChannelID, textChannelID)
		responseText := b.formatUnsubscribeResponse(s, voiceChannelID, true)

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: responseText,
				Flags:   discordgo.MessageFlagsEphemeral,
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
		channelName := b.getChannelName(s, channelID)
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

	removed := b.removeSubscription(voiceChannelID, textChannelID)
	responseText := b.formatUnsubscribeResponse(s, voiceChannelID, removed)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    responseText,
			Components: []discordgo.MessageComponent{}, // Remove the select menu
		},
	})
}

func (b *Bot) handleListSubscriptions(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guildID := i.GuildID
	channelID := i.ChannelID

	// Check if this is the admin channel
	b.mu.RLock()
	adminChannelID, hasAdminChannel := b.adminChannels[guildID]
	b.mu.RUnlock()

	if !hasAdminChannel {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ No admin channel has been set for this server. Please configure it using the ADMIN_CHANNELS environment variable.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	if channelID != adminChannelID {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("❌ This command can only be used in the admin channel: <#%s>", adminChannelID),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Build the subscription list
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.subscriptions) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "ℹ️ No active subscriptions in this server",
			},
		})
		return
	}

	// Build embed fields and select menu options
	var fields []*discordgo.MessageEmbedField
	var selectOptions []discordgo.SelectMenuOption

	count := 0
	for voiceChannelID, subs := range b.subscriptions {
		// Filter for this guild
		guildSubs := []subscription{}
		for _, sub := range subs {
			if sub.GuildId == guildID {
				guildSubs = append(guildSubs, sub)
			}
		}

		if len(guildSubs) == 0 {
			continue
		}

		voiceChannelName := b.getChannelName(s, voiceChannelID)
		var notifyChannels string
		for _, sub := range guildSubs {
			notifyChannels += fmt.Sprintf("→ <#%s>\n", sub.TextChannelId)
			count++
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("🔊 %s", voiceChannelName),
			Value:  notifyChannels,
			Inline: true,
		})

		// Add to select menu (limit 25 options)
		if len(selectOptions) < 25 {
			description := fmt.Sprintf("%d subscription(s)", len(guildSubs))
			selectOptions = append(selectOptions, discordgo.SelectMenuOption{
				Label:       voiceChannelName,
				Value:       voiceChannelID,
				Description: description,
				Emoji: &discordgo.ComponentEmoji{
					Name: "🔊",
				},
			})
		}
	}

	if count == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "ℹ️ No active subscriptions in this server",
			},
		})
		return
	}

	// Create components with select menu
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    "manage_subscription_select",
					Placeholder: "Select a voice channel to manage...",
					Options:     selectOptions,
				},
			},
		},
	}

	embed := &discordgo.MessageEmbed{
		Title:       "📋 Active Voice Channel Subscriptions",
		Description: fmt.Sprintf("**Total:** %d subscription(s) across %d voice channel(s)\n\nSelect a voice channel below to view and manage its subscriptions.", count, len(selectOptions)),
		Color:       0x5865F2, // Discord Blurple
		Fields:      fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Select a channel to remove specific subscriptions",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
		},
	})
}

func (b *Bot) handleManageSubscriptionSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	guildID := i.GuildID

	// Get the selected voice channel ID
	if len(data.Values) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ No channel selected",
			},
		})
		return
	}

	voiceChannelID := data.Values[0]
	voiceChannelName := b.getChannelName(s, voiceChannelID)

	// Get subscriptions for this voice channel
	b.mu.RLock()
	subs, exists := b.subscriptions[voiceChannelID]
	b.mu.RUnlock()

	if !exists {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("ℹ️ No subscriptions found for **%s**", voiceChannelName),
			},
		})
		return
	}

	// Filter for this guild
	var guildSubs []subscription
	for _, sub := range subs {
		if sub.GuildId == guildID {
			guildSubs = append(guildSubs, sub)
		}
	}

	if len(guildSubs) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("ℹ️ No subscriptions found for **%s** in this server", voiceChannelName),
			},
		})
		return
	}

	// Build buttons for each subscription
	var buttons []discordgo.MessageComponent
	var description string
	description = fmt.Sprintf("**Voice Channel:** 🔊 %s\n\n**Notification Channels:**\n", voiceChannelName)

	for idx, sub := range guildSubs {
		description += fmt.Sprintf("%d. <#%s>\n", idx+1, sub.TextChannelId)

		// Create remove button
		button := discordgo.Button{
			Label:    fmt.Sprintf("Remove #%d", idx+1),
			Style:    discordgo.DangerButton,
			CustomID: fmt.Sprintf("remove_sub:%s:%s", voiceChannelID, sub.TextChannelId),
		}
		buttons = append(buttons, button)
	}

	// Organize buttons into action rows (max 5 buttons per row)
	var components []discordgo.MessageComponent
	for i := 0; i < len(buttons); i += 5 {
		end := i + 5
		if end > len(buttons) {
			end = len(buttons)
		}
		components = append(components, discordgo.ActionsRow{
			Components: buttons[i:end],
		})
	}

	// Add a "Back" button
	components = append(components, discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "← Back to List",
				Style:    discordgo.SecondaryButton,
				CustomID: "back_to_subscription_list",
			},
		},
	})

	embed := &discordgo.MessageEmbed{
		Title:       "🔧 Manage Subscriptions",
		Description: description,
		Color:       0x5865F2,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Managing subscriptions for voice channel: %s", voiceChannelName),
		},
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
		},
	})
}

func (b *Bot) handleRemoveSubscriptionButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	guildID := i.GuildID

	// Parse the custom ID: "remove_sub:voiceChannelID:textChannelID"
	parts := strings.Split(data.CustomID, ":")
	if len(parts) != 3 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ Invalid button data",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	voiceChannelID := parts[1]
	textChannelID := parts[2]

	// Verify this is in the admin channel
	b.mu.RLock()
	adminChannelID, hasAdminChannel := b.adminChannels[guildID]
	b.mu.RUnlock()

	if !hasAdminChannel || i.ChannelID != adminChannelID {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ This action can only be performed in the admin channel",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Remove the subscription
	removed := b.removeSubscription(voiceChannelID, textChannelID)

	voiceChannelName := b.getChannelName(s, voiceChannelID)

	if removed {
		// Show success message with button to go back to list
		embed := &discordgo.MessageEmbed{
			Title:       "✅ Subscription Removed",
			Description: fmt.Sprintf("Successfully removed subscription:\n\n🔊 **%s** → <#%s>", voiceChannelName, textChannelID),
			Color:       0x57F287, // Green
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{embed},
				Components: []discordgo.MessageComponent{
					discordgo.ActionsRow{
						Components: []discordgo.MessageComponent{
							discordgo.Button{
								Label:    "← Back to List",
								Style:    discordgo.PrimaryButton,
								CustomID: "back_to_subscription_list",
							},
						},
					},
				},
			},
		})
	} else {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("ℹ️ Subscription not found: **%s** → <#%s>", voiceChannelName, textChannelID),
				Components: []discordgo.MessageComponent{
					discordgo.ActionsRow{
						Components: []discordgo.MessageComponent{
							discordgo.Button{
								Label:    "← Back to List",
								Style:    discordgo.SecondaryButton,
								CustomID: "back_to_subscription_list",
							},
						},
					},
				},
				Flags: discordgo.MessageFlagsEphemeral,
			},
		})
	}
}

func (b *Bot) handleBackToSubscriptionList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guildID := i.GuildID

	// Rebuild the subscription list view
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.subscriptions) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    "ℹ️ No active subscriptions in this server",
				Components: []discordgo.MessageComponent{},
			},
		})
		return
	}

	// Build embed fields and select menu options
	var fields []*discordgo.MessageEmbedField
	var selectOptions []discordgo.SelectMenuOption

	count := 0
	for voiceChannelID, subs := range b.subscriptions {
		// Filter for this guild
		guildSubs := []subscription{}
		for _, sub := range subs {
			if sub.GuildId == guildID {
				guildSubs = append(guildSubs, sub)
			}
		}

		if len(guildSubs) == 0 {
			continue
		}

		voiceChannelName := b.getChannelName(s, voiceChannelID)
		var notifyChannels string
		for _, sub := range guildSubs {
			notifyChannels += fmt.Sprintf("→ <#%s>\n", sub.TextChannelId)
			count++
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("🔊 %s", voiceChannelName),
			Value:  notifyChannels,
			Inline: true,
		})

		// Add to select menu (limit 25 options)
		if len(selectOptions) < 25 {
			description := fmt.Sprintf("%d subscription(s)", len(guildSubs))
			selectOptions = append(selectOptions, discordgo.SelectMenuOption{
				Label:       voiceChannelName,
				Value:       voiceChannelID,
				Description: description,
				Emoji: &discordgo.ComponentEmoji{
					Name: "🔊",
				},
			})
		}
	}

	if count == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    "ℹ️ No active subscriptions in this server",
				Components: []discordgo.MessageComponent{},
			},
		})
		return
	}

	// Create components with select menu
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    "manage_subscription_select",
					Placeholder: "Select a voice channel to manage...",
					Options:     selectOptions,
				},
			},
		},
	}

	embed := &discordgo.MessageEmbed{
		Title:       "📋 Active Voice Channel Subscriptions",
		Description: fmt.Sprintf("**Total:** %d subscription(s) across %d voice channel(s)\n\nSelect a voice channel below to view and manage its subscriptions.", count, len(selectOptions)),
		Color:       0x5865F2, // Discord Blurple
		Fields:      fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Select a channel to remove specific subscriptions",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
		},
	})
}

// loadPersistedData loads subscriptions and admin channels from disk
func (b *Bot) loadPersistedData() error {
	data, err := b.persistence.Load()
	if err != nil {
		return err
	}

	b.mu.Lock()
	b.subscriptions = data.Subscriptions
	// Only load admin channels from disk if not set via environment
	if len(b.adminChannels) == 0 {
		b.adminChannels = data.AdminChannels
	}
	b.mu.Unlock()

	log.Printf("Loaded %d voice channel subscriptions and %d admin channels", len(data.Subscriptions), len(b.adminChannels))
	return nil
}

// loadAdminChannelsFromEnv loads admin channels from ADMIN_CHANNELS environment variable
// Format: ADMIN_CHANNELS=guildID:channelID,guildID:channelID
func (b *Bot) loadAdminChannelsFromEnv() {
	envAdminChannels := os.Getenv("ADMIN_CHANNELS")
	if envAdminChannels == "" {
		return
	}

	pairs := strings.Split(envAdminChannels, ",")
	count := 0
	for _, pair := range pairs {
		parts := strings.Split(strings.TrimSpace(pair), ":")
		if len(parts) == 2 {
			guildID := strings.TrimSpace(parts[0])
			channelID := strings.TrimSpace(parts[1])
			if guildID != "" && channelID != "" {
				b.mu.Lock()
				b.adminChannels[guildID] = channelID
				b.mu.Unlock()
				count++
			}
		}
	}

	if count > 0 {
		log.Printf("Loaded %d admin channels from ADMIN_CHANNELS environment variable", count)
	}
}

// savePersistedData saves subscriptions and admin channels to disk
func (b *Bot) savePersistedData() error {
	b.mu.RLock()
	data := &PersistentData{
		Subscriptions: b.subscriptions,
		AdminChannels: b.adminChannels,
	}
	b.mu.RUnlock()

	return b.persistence.Save(data)
}

// savePersistedDataAsync saves subscriptions and admin channels to disk asynchronously
func (b *Bot) savePersistedDataAsync() {
	go func() {
		if err := b.savePersistedData(); err != nil {
			log.Printf("Error saving persisted data: %v", err)
		}
	}()
}

// addSubscription adds a subscription and returns whether it already existed
func (b *Bot) addSubscription(voiceChannelID, textChannelID, guildID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.subscriptions[voiceChannelID] == nil {
		b.subscriptions[voiceChannelID] = []subscription{}
	}

	// Check if already subscribed
	for _, sub := range b.subscriptions[voiceChannelID] {
		if sub.TextChannelId == textChannelID && sub.VoiceChannelId == voiceChannelID {
			return true
		}
	}

	// Add new subscription
	b.subscriptions[voiceChannelID] = append(b.subscriptions[voiceChannelID], subscription{
		VoiceChannelId: voiceChannelID,
		TextChannelId:  textChannelID,
		GuildId:        guildID,
	})
	b.mu.Unlock()

	// Save to persistence asynchronously (non-blocking)
	b.savePersistedDataAsync()

	return false
}

// removeSubscription removes a subscription and returns whether it existed
func (b *Bot) removeSubscription(voiceChannelID, textChannelID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs, exists := b.subscriptions[voiceChannelID]
	if !exists {
		return false
	}

	for idx, sub := range subs {
		if sub.TextChannelId == textChannelID && sub.VoiceChannelId == voiceChannelID {
			// Remove this subscription
			b.subscriptions[voiceChannelID] = append(subs[:idx], subs[idx+1:]...)

			// Clean up empty subscription lists
			if len(b.subscriptions[voiceChannelID]) == 0 {
				delete(b.subscriptions, voiceChannelID)
			}

			// Save to persistence asynchronously (non-blocking)
			b.mu.Unlock()
			b.savePersistedDataAsync()
			b.mu.Lock()

			return true
		}
	}
	return false
}

// getChannelName fetches the channel name or returns the ID if fetching fails
func (b *Bot) getChannelName(s *discordgo.Session, channelID string) string {
	channel, err := s.Channel(channelID)
	if err == nil {
		return channel.Name
	}
	return channelID
}

// formatSubscribeResponse generates the response message for subscribe operations
func (b *Bot) formatSubscribeResponse(s *discordgo.Session, voiceChannelID string, alreadySubscribed bool) string {
	channelName := b.getChannelName(s, voiceChannelID)

	if alreadySubscribed {
		return fmt.Sprintf("ℹ️ Already subscribed to **%s**", channelName)
	}
	return fmt.Sprintf("✅ Subscribed! This channel will receive notifications for voice activity in **%s**", channelName)
}

// formatUnsubscribeResponse generates the response message for unsubscribe operations
func (b *Bot) formatUnsubscribeResponse(s *discordgo.Session, voiceChannelID string, wasSubscribed bool) string {
	channelName := b.getChannelName(s, voiceChannelID)

	if !wasSubscribed {
		return fmt.Sprintf("ℹ️ Not subscribed to **%s**", channelName)
	}
	return fmt.Sprintf("✅ Unsubscribed from **%s**", channelName)
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
		_, err := s.ChannelMessageSend(sub.TextChannelId, message)
		if err != nil {
			log.Printf("Error sending notification to channel %v: %v", sub.TextChannelId, err)
		}
	}
}
