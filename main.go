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

// --- Rate limiting and duplicate prevention globals ---
var (
	rateLimitMu  sync.Mutex
	rateLimitMap = make(map[string][]int64) // userID -> timestamps

	transcribedMu     sync.Mutex
	transcribedMsgIDs = make(map[string]struct{}) // messageID -> struct{}
	transcribedOrder  []string                    // maintain order of insertion for eviction
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

	// --- Rate limiting: 3 uses per 5 minutes per user ---
	const rateLimitCount = 3
	const rateLimitWindow = 5 * 60 // seconds

	isRateLimited := func(userID string) (bool, int) {
		rateLimitMu.Lock()
		defer rateLimitMu.Unlock()
		now := time.Now().Unix()
		windowStart := now - rateLimitWindow
		times := rateLimitMap[userID]
		// Remove timestamps outside window
		var filtered []int64
		for _, t := range times {
			if t >= windowStart {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) >= rateLimitCount {
			// Calculate minutes remaining
			oldest := filtered[0]
			resetIn := int((oldest + rateLimitWindow) - now)
			if resetIn < 0 {
				resetIn = 0
			}
			return true, (resetIn + 59) / 60 // round up to next minute
		}
		// Not limited, add this timestamp
		filtered = append(filtered, now)
		rateLimitMap[userID] = filtered
		return false, 0
	}

	limited, mins := isRateLimited(m.Author.ID)
	if limited {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("rate limit exceeded. counter resets in %d minutes", mins))
		return
	}

	// Only respond to !transcribe
	if strings.TrimSpace(m.Content) != "!transcribe" {
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

	// --- Prevent duplicate transcriptions ---
	var audioMsgID string
	if targetMsg != nil {
		audioMsgID = targetMsg.ID
	}
	if audioMsgID != "" {
		transcribedMu.Lock()
		_, already := transcribedMsgIDs[audioMsgID]
		if already {
			transcribedMu.Unlock()
			s.ChannelMessageSend(m.ChannelID, "This audio message has already been transcribed.")
			return
		}
		// Mark as transcribed
		transcribedMsgIDs[audioMsgID] = struct{}{}
		transcribedOrder = append(transcribedOrder, audioMsgID)
		// If over 1000, evict oldest
		if len(transcribedOrder) > 1000 {
			oldest := transcribedOrder[0]
			delete(transcribedMsgIDs, oldest)
			transcribedOrder = transcribedOrder[1:]
		}
		transcribedMu.Unlock()
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

	const maxDiscordMsgLen = 4000

	// Helper to split at word boundaries
	splitMessage := func(s string, maxLen int) []string {
		var result []string
		runes := []rune(s)
		for len(runes) > 0 {
			if len(runes) <= maxLen {
				result = append(result, string(runes))
				break
			}
			// Find last space before maxLen
			cut := maxLen
			for i := maxLen; i > 0; i-- {
				if runes[i] == ' ' || runes[i] == '\n' {
					cut = i
					break
				}
			}
			if cut == 0 {
				cut = maxLen // no space found, hard cut
			}
			result = append(result, string(runes[:cut]))
			runes = runes[cut:]
			// Trim leading spaces/newlines
			for len(runes) > 0 && (runes[0] == ' ' || runes[0] == '\n') {
				runes = runes[1:]
			}
		}
		return result
	}

	// --- Language detection and translation ---
	detectAndTranslate := func(text string) (lang string, translation string, err error) {
		apiKey := os.Getenv("GOOGLE_API_KEY")
		if apiKey == "" {
			return "", "", fmt.Errorf("GOOGLE_API_KEY not set")
		}
		ctx := context.Background()
		client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
		if err != nil {
			return "", "", err
		}
		defer client.Close()

		model := client.GenerativeModel("gemini-1.5-pro")

		// Detect language
		promptDetect := []genai.Part{
			genai.Text("What is the ISO 639-1 language code of the following text? Only output the code, nothing else.\n" + text),
		}
		resp, err := model.GenerateContent(ctx, promptDetect...)
		if err != nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			return "", "", fmt.Errorf("language detection failed")
		}
		lang = strings.TrimSpace(fmt.Sprint(resp.Candidates[0].Content.Parts[0]))

		if lang == "en" {
			return lang, "", nil
		}

		// Translate to English
		promptTrans := []genai.Part{
			genai.Text("Translate this to English:\n" + text),
		}
		resp2, err := model.GenerateContent(ctx, promptTrans...)
		if err != nil || len(resp2.Candidates) == 0 || resp2.Candidates[0].Content == nil {
			return lang, "", fmt.Errorf("translation failed")
		}
		translation = ""
		for _, part := range resp2.Candidates[0].Content.Parts {
			if t, ok := part.(genai.Text); ok {
				translation += string(t)
			}
		}
		return lang, translation, nil
	}

	replyText := username + " said, \"" + transcript + "\""
	lang, translation, terr := detectAndTranslate(transcript)

	parts := splitMessage(replyText, maxDiscordMsgLen)
	ref := &discordgo.MessageReference{
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		GuildID:   m.GuildID,
	}
	if targetMsg != nil {
		ref.MessageID = targetMsg.ID
	}

	for i, part := range parts {
		msgSend := &discordgo.MessageSend{Content: part}
		if i == 0 {
			msgSend.Reference = ref
		}
		_, err = s.ChannelMessageSendComplex(m.ChannelID, msgSend)
		if err != nil {
			log.Printf("Failed to send transcript reply: %v", err)
			botLogger.Errorf("Failed to send transcript reply for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
			break
		}
	}

	// If not English and translation succeeded, send translation as a new message
	if lang != "en" && translation != "" && terr == nil {
		transParts := splitMessage("Translation: "+translation, maxDiscordMsgLen)
		for _, part := range transParts {
			_, err = s.ChannelMessageSend(m.ChannelID, part)
			if err != nil {
				log.Printf("Failed to send translation: %v", err)
				botLogger.Errorf("Failed to send translation for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
				break
			}
		}
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

	model := client.GenerativeModel("gemini-1.5-pro")

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
