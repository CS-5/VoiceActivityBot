package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/CS-5/VoiceActivityBot/bot"
)

func main() {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN environment variable is required")
	}

	bot, err := bot.NewBot(token)
	if err != nil {
		log.Fatal("Error creating bot:", err)
	}

	err = bot.Start()
	if err != nil {
		log.Fatal("Error starting bot:", err)
	}

	log.Println("Bot is now running. SIGINT, SIGTERM, or CTRL+C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	// Cleanup: unregister commands
	log.Println("Shutting down, cleaning up commands...")
	bot.Stop()
}
