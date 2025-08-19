package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/generative-ai-go/genai"
	"github.com/joho/godotenv"
	"google.golang.org/api/option"
)

// --- Rate limiting and deduplication globals ---
var (
	rateLimitMu              sync.Mutex
	userTranscribeTimestamps = make(map[string][]time.Time) // userID -> slice of timestamps

	dedupMu           sync.Mutex
	transcribedMsgIDs = make([]string, 0, 1000)   // FIFO queue of message IDs
	transcribedMsgSet = make(map[string]struct{}) // for fast lookup
)

// Logger struct for in-memory logging
type Logger struct {
	mu   sync.Mutex
	logs []string
}

func (l *Logger) Logf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := fmt.Sprintf("[INFO] "+format, args...)
	l.logs = append(l.logs, entry)
	fmt.Println(entry)
}

func (l *Logger) Errorf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := fmt.Sprintf("[ERROR] "+format, args...)
	l.logs = append(l.logs, entry)
	fmt.Println(entry)
}

func (l *Logger) SaveToFile() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.logs) == 0 {
		return nil
	}
	date := time.Now().Format("2006-01-02")
	logDir := "logs"
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		os.Mkdir(logDir, 0755)
	}
	filePath := filepath.Join(logDir, fmt.Sprintf("%s-log.txt", date))
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, entry := range l.logs {
		f.WriteString(entry + "\n")
	}
	return nil
}

var botLogger = &Logger{}

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

// // enhanceTranscriptWithGenAI refines a transcript using Gemini
// func enhanceTranscriptWithGenAI(raw string) (string, error) {
// 	apiKey := os.Getenv("GOOGLE_API_KEY")
// 	if apiKey == "" {
// 		return "", os.ErrNotExist
// 	}
// 	ctx := context.Background()
// 	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
// 	if err != nil {
// 		return "", err
// 	}
// 	defer client.Close()

// 	model := client.GenerativeModel("gemini-1.5-pro")
// 	prompt := []genai.Part{
// 		genai.Text("You are a helpful assistant that refines speech transcripts for clarity, grammar, and conciseness. Refine this transcript: " + raw),
// 	}
// 	resp, err := model.GenerateContent(ctx, prompt...)
// 	if err != nil {
// 		return "", err
// 	}
// 	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
// 		return "", nil
// 	}
// 	var enhanced string
// 	for _, part := range resp.Candidates[0].Content.Parts {
// 		if t, ok := part.(genai.Text); ok {
// 			enhanced += string(t)
// 		}
// 	}
// 	return enhanced, nil
// }

func main() {
	// Load environment variables from .env
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found or error loading .env:", err)
		botLogger.Logf("No .env file found or error loading .env: %v", err)
	}

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		botLogger.Errorf("DISCORD_TOKEN not set in environment")
		botLogger.SaveToFile()
		log.Fatal("DISCORD_TOKEN not set in environment")
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		botLogger.Errorf("Error creating Discord session: %v", err)
		botLogger.SaveToFile()
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.AddHandler(messageCreate)

	if err := dg.Open(); err != nil {
		botLogger.Errorf("Error opening Discord session: %v", err)
		botLogger.SaveToFile()
		log.Fatalf("Error opening Discord session: %v", err)
	}
	log.Println("Bot is now running. Press CTRL+C to exit.")
	botLogger.Logf("Bot started and running.")

	// Wait for a termination signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-stop

	log.Println("Shutting down...")
	botLogger.Logf("Bot shutting down.")
	dg.Close()
	if err := botLogger.SaveToFile(); err != nil {
		log.Printf("Failed to save log: %v", err)
	}
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// --- Deduplication: skip if this message was already transcribed ---
	dedupMu.Lock()
	if _, exists := transcribedMsgSet[m.ID]; exists {
		dedupMu.Unlock()
		s.ChannelMessageSend(m.ChannelID, "This message was already transcribed.")
		return
	}
	// Add this message ID to the set and queue
	transcribedMsgIDs = append(transcribedMsgIDs, m.ID)
	transcribedMsgSet[m.ID] = struct{}{}
	// Maintain max size 1000
	if len(transcribedMsgIDs) > 1000 {
		oldest := transcribedMsgIDs[0]
		transcribedMsgIDs = transcribedMsgIDs[1:]
		delete(transcribedMsgSet, oldest)
	}
	dedupMu.Unlock()

	// Only respond to exact '!transcribe' from users (not bots)
	if m.Author.ID == s.State.User.ID || m.Content != "!transcribe" {
		return
	}

	// --- Rate limit: 1 use per minute per user, except for 'itsmine12' ---
	if m.Author.Username != "itsmine12" {
		rateLimitMu.Lock()
		now := time.Now()
		times := userTranscribeTimestamps[m.Author.ID]
		// Remove timestamps older than 1 minute
		var recent []time.Time
		for _, t := range times {
			if now.Sub(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		// Always update the timestamps, even if rate limited
		userTranscribeTimestamps[m.Author.ID] = recent
		if len(recent) >= 1 {
			// Only send a rate limit message if this is the first over-limit message in the current minute
			alreadyWarned := false
			if len(recent) > 0 && now.Sub(recent[len(recent)-1]) < 5*time.Second {
				alreadyWarned = true
			}
			if !alreadyWarned {
				s.ChannelMessageSend(m.ChannelID, "Rate limit: Max 1 transcription per minute. Please wait.")
			}
			rateLimitMu.Unlock()
			return
		}
		// Add this attempt
		userTranscribeTimestamps[m.Author.ID] = append(recent, now)
		rateLimitMu.Unlock()
	}

	var targetMsg *discordgo.Message
	var att *discordgo.MessageAttachment

	// If this message is a reply, check the referenced message
	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		refMsg, err := s.ChannelMessage(m.ChannelID, m.MessageReference.MessageID)
		if err == nil && len(refMsg.Attachments) > 0 {
			for _, a := range refMsg.Attachments {
				name := strings.ToLower(a.Filename)
				if strings.HasSuffix(name, ".ogg") || strings.HasSuffix(name, ".mp3") || strings.HasSuffix(name, ".wav") {
					targetMsg = refMsg
					att = a
					break
				}
			}
		}
	}

	// If not found in reply, search last 5 messages in channel for the most recent voice note
	if att == nil {
		msgs, err := s.ChannelMessages(m.ChannelID, 5, "", "", "")
		if err == nil {
			// Iterate in reverse to get the most recent message with a voice note
			for i := len(msgs) - 1; i >= 0; i-- {
				msg := msgs[i]
				if msg.ID == m.ID {
					continue
				}
				for _, a := range msg.Attachments {
					name := strings.ToLower(a.Filename)
					if strings.HasSuffix(name, ".ogg") || strings.HasSuffix(name, ".mp3") || strings.HasSuffix(name, ".wav") {
						targetMsg = msg
						att = a
						break
					}
				}
				if att != nil {
					break
				}
			}
		}
	}

	if att == nil {
		botLogger.Logf("Transcription request: user=%s (%s), targetUser=none, transcript=", m.Author.Username, m.Author.ID)
		s.ChannelMessageSend(m.ChannelID, "No attachment found")
		botLogger.Logf("User %s (%s) tried to transcribe but no attachment found.", m.Author.Username, m.Author.ID)
		return
	}

	tmpFile, err := downloadFile(att.URL)
	if err != nil {
		botLogger.Logf("Transcription request: user=%s (%s), targetUser=unknown, transcript=", m.Author.Username, m.Author.ID)
		log.Printf("Download error: %v", err)
		botLogger.Errorf("Download error for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
		s.ChannelMessageSend(m.ChannelID, "Error transcribing: "+err.Error())
		return
	}

	// Always remove the temp file, even if transcription fails
	var transcript string
	func() {
		defer func() {
			if err := os.Remove(tmpFile); err != nil {
				log.Printf("Failed to remove temp file: %v", err)
				botLogger.Errorf("Failed to remove temp file: %v", err)
			}
		}()
		transcript, err = transcribe(tmpFile)
	}()

	username := "Someone"
	userID := ""
	if targetMsg != nil && targetMsg.Author != nil {
		username = targetMsg.Author.Username
		userID = targetMsg.Author.ID
	}

	// Log every transcription request, even if blank
	botLogger.Logf("Transcription request: user=%s (%s), targetUser=%s (%s), transcript=%q", m.Author.Username, m.Author.ID, username, userID, transcript)

	if err != nil {
		log.Printf("Transcription failed: %v", err)
		botLogger.Errorf("Transcription failed for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
		s.ChannelMessageSend(m.ChannelID, "Error transcribing: "+err.Error())
		return
	}

	// If transcript is empty, send nothing
	if strings.TrimSpace(transcript) == "" {
		// Already logged above
		return
	}

	replyText := username + " said, \"" + transcript + "\""

	// Discord message character limit (actual limit is 2000)
	const maxLen = 2000
	var chunks []string
	if len(replyText) <= maxLen {
		chunks = []string{replyText}
	} else {
		words := strings.Fields(replyText)
		var current strings.Builder
		for _, word := range words {
			// +1 for space if not first word
			addLen := len(word)
			if current.Len() > 0 {
				addLen++
			}
			if current.Len()+addLen > maxLen {
				chunks = append(chunks, current.String())
				current.Reset()
				current.WriteString(word)
			} else {
				if current.Len() > 0 {
					current.WriteByte(' ')
				}
				current.WriteString(word)
			}
		}
		if current.Len() > 0 {
			chunks = append(chunks, current.String())
		}
	}

	ref := &discordgo.MessageReference{
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		GuildID:   m.GuildID,
	}
	if targetMsg != nil {
		ref.MessageID = targetMsg.ID
	}
	for _, chunk := range chunks {
		_, err = s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:   chunk,
			Reference: ref,
		})
		if err != nil {
			log.Printf("Failed to send transcript reply: %v", err)
			botLogger.Errorf("Failed to send transcript reply for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
		}
		// Only reference the original message for the first chunk
		ref = nil
	}
}

// transcribe calls transcribeAudio and logs errors
func transcribe(filePath string) (string, error) {
	transcript, err := transcribeAudio(filePath)
	if err != nil {
		log.Printf("transcribeAudio error: %v", err)
		botLogger.Errorf("transcribeAudio error: %v", err)
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

	model := client.GenerativeModel("gemini-2.5-flash-lite")

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

	// Use a preallocated slice for prompt
	prompt := make([]genai.Part, 2)
	prompt[0] = genai.Text("Transcribe this audio file accurately:")
	prompt[1] = genai.Blob{
		MIMEType: mimeType,
		Data:     data,
	}

	resp, err := model.GenerateContent(ctx, prompt...)
	if err != nil {
		return "", err
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return "", nil
	}

	// Use strings.Builder for efficient string concatenation
	var sb strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if t, ok := part.(genai.Text); ok {
			sb.WriteString(string(t))
		}
	}

	return sb.String(), nil
}
