package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/google/generative-ai-go/genai"
	"github.com/joho/godotenv"
	"google.golang.org/api/option"
)

// downloadFile downloads a file from a URL to a temp file and returns the local path
func downloadFile(url string) (string, error) {
	base := filepath.Base(url)
	// Remove query parameters from filename (everything after '?')
	if idx := strings.Index(base, "?"); idx != -1 {
		base = base[:idx]
	}
	tmpPath := filepath.Join(os.TempDir(), base)

	resp, err := http.Get(url)
	if err != nil {
		log.Printf("Failed to download %s: %v", url, err)
		return "", err
	}
	defer resp.Body.Close()

	out, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("Failed to create temp file: %v", err)
		return "", err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		log.Printf("Failed to save file: %v", err)
		return "", err
	}

	return tmpPath, nil
}

// enhanceTranscriptWithGenAI refines a transcript using Gemini
func enhanceTranscriptWithGenAI(raw string) (string, error) {
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		return "", os.ErrNotExist
	}
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return "", err
	}
	defer client.Close()

	model := client.GenerativeModel("gemini-1.5-pro")
	prompt := []genai.Part{
		genai.Text("You are a helpful assistant that refines speech transcripts for clarity, grammar, and conciseness. Refine this transcript: " + raw),
	}
	resp, err := model.GenerateContent(ctx, prompt...)
	if err != nil {
		return "", err
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return "", nil
	}
	var enhanced string
	for _, part := range resp.Candidates[0].Content.Parts {
		if t, ok := part.(genai.Text); ok {
			enhanced += string(t)
		}
	}
	return enhanced, nil
}

func main() {
	// Load environment variables from .env
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found or error loading .env:", err)
	}

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN not set in environment")
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.AddHandler(messageCreate)

	if err := dg.Open(); err != nil {
		log.Fatalf("Error opening Discord session: %v", err)
	}
	log.Println("Bot is now running. Press CTRL+C to exit.")

	// Wait for a termination signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-stop

	log.Println("Shutting down...")
	dg.Close()
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore messages from the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	if len(m.Attachments) == 0 {
		return
	}

	for _, att := range m.Attachments {
		name := strings.ToLower(att.Filename)
		// Only process audio files (voice messages are usually .ogg, often named like 'voice-message.ogg')
		if !(strings.HasSuffix(name, ".ogg") || strings.HasSuffix(name, ".mp3") || strings.HasSuffix(name, ".wav")) {
			continue // skip non-audio
		}

		tmpFile, err := downloadFile(att.URL)
		if err != nil {
			log.Printf("Download error: %v", err)
			s.ChannelMessageSend(m.ChannelID, "Error transcribing: "+err.Error())
			continue
		}
		defer os.Remove(tmpFile)

		transcript, err := transcribe(tmpFile)
		if err != nil {
			log.Printf("Transcription failed: %v", err)
			s.ChannelMessageSend(m.ChannelID, "Error transcribing: "+err.Error())
			continue
		}

		_, err = s.ChannelMessageSend(m.ChannelID, transcript)
		if err != nil {
			log.Printf("Failed to send transcript: %v", err)
		}
	}
}

// transcribe calls transcribeAudio and logs errors
func transcribe(filePath string) (string, error) {
	transcript, err := transcribeAudio(filePath)
	if err != nil {
		log.Printf("transcribeAudio error: %v", err)
	}
	return transcript, err
}

// transcribeAudio uses Google Gemini API to transcribe audio
func transcribeAudio(filePath string) (string, error) {
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		return "", os.ErrNotExist
	}
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return "", err
	}
	defer client.Close()

	model := client.GenerativeModel("gemini-2.5-pro")

	// Read audio file bytes
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	// Guess MIME type from extension
	mimeType := "audio/wav"
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".mp3":
		mimeType = "audio/mpeg"
	case ".ogg":
		mimeType = "audio/ogg"
	case ".wav":
		mimeType = "audio/wav"
	}

	prompt := []genai.Part{
		genai.Text("Transcribe this audio file accurately:"),
		genai.Blob{
			MIMEType: mimeType,
			Data:     data,
		},
	}

	resp, err := model.GenerateContent(ctx, prompt...)
	if err != nil {
		return "", err
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return "", nil
	}

	var transcript string
	for _, part := range resp.Candidates[0].Content.Parts {
		if t, ok := part.(genai.Text); ok {
			transcript += string(t)
		}
	}

	// Clean up the file after transcription
	os.Remove(filePath)

	return transcript, nil
}
