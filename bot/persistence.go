package bot

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

type (
	// PersistentData represents the data structure to be saved to disk
	PersistentData struct {
		Subscriptions map[string][]subscription `json:"subscriptions"`
		AdminChannels map[string]string         `json:"admin_channels"` // guildID -> channelID
	}

	// Persistence handles reading and writing bot state to disk
	Persistence struct {
		filePath string
		mu       sync.Mutex
	}
)

// NewPersistence creates a new persistence handler
func NewPersistence(filePath string) *Persistence {
	if filePath == "" {
		filePath = "subscriptions.json"
	}
	return &Persistence{
		filePath: filePath,
	}
}

// Load reads the persistent data from disk
func (p *Persistence) Load() (*PersistentData, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	data := &PersistentData{
		Subscriptions: make(map[string][]subscription),
		AdminChannels: make(map[string]string),
	}

	file, err := os.ReadFile(p.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist yet, return empty data
			return data, nil
		}
		return nil, err
	}

	err = json.Unmarshal(file, data)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// Save writes the persistent data to disk
func (p *Persistence) Save(data *PersistentData) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	err = os.WriteFile(p.filePath, jsonData, 0644)
	if err != nil {
		return err
	}

	log.Printf("Saved %d subscriptions to %s", len(data.Subscriptions), p.filePath)
	return nil
}
