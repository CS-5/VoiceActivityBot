package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/bwmarrin/discordgo"
)

// Subscription represents a voice channel subscription
type Subscription struct {
	VoiceChannelID string
	TextChannelID  string
	GuildID        string
}

// Bot holds the bot state
type Bot struct {
	session       *discordgo.Session
	subscriptions map[string][]Subscription // key: voiceChannelID
	mu            sync.RWMutex
}

var bot *Bot

func main() {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN environment variable is required")
	}

	// Create a new Discord session
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal("Error creating Discord session:", err)
	}

	bot = &Bot{
		session:       dg,
		subscriptions: make(map[string][]Subscription),
	}

	// Register event handlers
	dg.AddHandler(ready)
	dg.AddHandler(voiceStateUpdate)
	dg.AddHandler(interactionCreate)

	// Set intents - we need GuildVoiceStates and Guilds
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates

	// Open connection
	err = dg.Open()
	if err != nil {
		log.Fatal("Error opening connection:", err)
	}
	defer dg.Close()

	log.Println("Bot is now running. Press CTRL+C to exit.")

	// Wait here until CTRL+C or other term signal is received
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)

	// Register slash commands for each guild
	for _, guild := range event.Guilds {
		registerCommands(s, guild.ID)
	}
}

func registerCommands(s *discordgo.Session, guildID string) {
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
		_, err := s.ApplicationCommandCreate(s.State.User.ID, guildID, cmd)
		if err != nil {
			log.Printf("Cannot create '%v' command in guild %v: %v", cmd.Name, guildID, err)
		}
	}
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type == discordgo.InteractionApplicationCommand {
		data := i.ApplicationCommandData()

		switch data.Name {
		case "subscribe":
			handleSubscribe(s, i)
		}
	}
}

func handleSubscribe(s *discordgo.Session, i *discordgo.InteractionCreate) {
	options := i.ApplicationCommandData().Options

	// Get the text channel where the command was issued
	textChannelID := i.ChannelID
	guildID := i.GuildID

	// Check if a voice channel was provided
	if len(options) == 0 {
		// No voice channel provided - show selection dialog
		handleSubscribeWithDialog(s, i)
		return
	}

	// Voice channel was provided
	voiceChannelID := options[0].ChannelValue(s).ID

	// Add subscription
	bot.mu.Lock()
	if bot.subscriptions[voiceChannelID] == nil {
		bot.subscriptions[voiceChannelID] = []Subscription{}
	}

	// Check if already subscribed
	alreadySubscribed := false
	for _, sub := range bot.subscriptions[voiceChannelID] {
		if sub.TextChannelID == textChannelID && sub.VoiceChannelID == voiceChannelID {
			alreadySubscribed = true
			break
		}
	}

	if !alreadySubscribed {
		bot.subscriptions[voiceChannelID] = append(bot.subscriptions[voiceChannelID], Subscription{
			VoiceChannelID: voiceChannelID,
			TextChannelID:  textChannelID,
			GuildID:        guildID,
		})
	}
	bot.mu.Unlock()

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

func handleSubscribeWithDialog(s *discordgo.Session, i *discordgo.InteractionCreate) {
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

	// Note: Handling the select menu response would require additional handler
	// For now, users can use /subscribe <channel> directly
}

func voiceStateUpdate(s *discordgo.Session, vsu *discordgo.VoiceStateUpdate) {
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
				sendNotifications(s, oldChannelID, oldMessage)
			}
		}
	}

	if message != "" && channelID != "" {
		sendNotifications(s, channelID, message)
	}
}

func sendNotifications(s *discordgo.Session, voiceChannelID string, message string) {
	bot.mu.RLock()
	subscriptions := bot.subscriptions[voiceChannelID]
	bot.mu.RUnlock()

	for _, sub := range subscriptions {
		_, err := s.ChannelMessageSend(sub.TextChannelID, message)
		if err != nil {
			log.Printf("Error sending notification to channel %v: %v", sub.TextChannelID, err)
		}
	}
}
