package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

// Helper to split at word boundaries
const maxDiscordMsgLen = 4000

func splitMessage(s string, maxLen int) []string {
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

// fetchSpeechmaticsTranscript fetches transcript for a given job ID
func fetchSpeechmaticsTranscript(jobID string) (string, error) {
	apiKey := os.Getenv("SPEECHMATICS_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("SPEECHMATICS_API_KEY not set in environment")
	}
	transcriptURL := "https://asr.api.speechmatics.com/v2/jobs/" + jobID + "/transcript?format=txt"
	req, err := http.NewRequest("GET", transcriptURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	transcript, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	return string(transcript), nil
}

// transcribeWithJobID returns transcript and Speechmatics job ID
func transcribeWithJobID(filePath string) (string, string, error) {
	apiKey := os.Getenv("SPEECHMATICS_API_KEY")
	if apiKey == "" {
		return "", "", fmt.Errorf("SPEECHMATICS_API_KEY not set in environment")
	}

	// Open audio file
	audioFile, err := os.Open(filePath)
	if err != nil {
		return "", "", err
	}
	defer audioFile.Close()

	// Prepare multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add config part
	config := `{"type": "transcription", "transcription_config": {"language": "auto", "diarization": "none", "operating_point": "enhanced", "enable_entities": true}}`
	if err := writer.WriteField("config", config); err != nil {
		return "", "", err
	}

	// Add audio file part (must be named "data_file")
	audioPart, err := writer.CreateFormFile("data_file", filepath.Base(filePath))
	if err != nil {
		return "", "", err
	}
	if _, err := io.Copy(audioPart, audioFile); err != nil {
		return "", "", err
	}

	writer.Close()

	req, err := http.NewRequest("POST", "https://asr.api.speechmatics.com/v2/jobs", &buf)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	// Parse job response
	type jobResp struct {
		ID     string `json:"id"`
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Code   int    `json:"code"`
	}
	var jr jobResp
	if err := json.Unmarshal(respBody, &jr); err != nil {
		return "", "", fmt.Errorf("failed to parse Speechmatics job response: %v", err)
	}
	if jr.Error != "" || jr.Code != 0 {
		// Submission error
		return "", "", fmt.Errorf("speechmatics submission error: %s (%s)", jr.Error, jr.Detail)
	}
	if jr.ID == "" {
		return "", "", fmt.Errorf("no job ID returned from Speechmatics")
	}

	statusURL := "https://asr.api.speechmatics.com/v2/jobs/" + jr.ID
	transcriptURL := statusURL + "/transcript?format=txt"
	for {
		time.Sleep(1 * time.Second)
		req, err := http.NewRequest("GET", statusURL, nil)
		if err != nil {
			return "", jr.ID, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return "", jr.ID, err
		}
		statusBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", jr.ID, err
		}
		var statusObj struct {
			Job struct {
				Status string `json:"status"`
				Errors []struct {
					Message   string `json:"message"`
					Timestamp string `json:"timestamp"`
				} `json:"errors"`
			} `json:"job"`
		}
		json.Unmarshal(statusBody, &statusObj)
		statusLower := strings.ToLower(statusObj.Job.Status)
		switch statusLower {
		case "done":
			req, err := http.NewRequest("GET", transcriptURL, nil)
			if err != nil {
				return "", jr.ID, err
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)
			resp, err := client.Do(req)
			if err != nil {
				return "", jr.ID, err
			}
			transcript, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return "", jr.ID, err
			}
			return string(transcript), jr.ID, nil
		case "rejected":
			var errMsg string
			if len(statusObj.Job.Errors) > 0 {
				errMsg = statusObj.Job.Errors[0].Message
			}
			return "", jr.ID, fmt.Errorf("speechmatics job rejected: %s", errMsg)
		case "deleted":
			return "", jr.ID, fmt.Errorf("speechmatics job deleted before completion")
		case "expired":
			return "", jr.ID, fmt.Errorf("speechmatics job expired before completion")
		case "running":
			// continue polling
		default:
			return "", jr.ID, fmt.Errorf("speechmatics job in unknown state: %s", statusObj.Job.Status)
		}
	}
}

// --- Rate limiting and duplicate prevention globals ---
// var (
// --- Language detection and translation ---
// Uses OpenRouter Grok 4 Fast for detection and translation
func detectAndTranslate(text string) (lang string, translation string, err error) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return "", "", fmt.Errorf("OPENROUTER_API_KEY not set in environment")
	}

	// 1. Detect language
	detectPrompt := map[string]interface{}{
		"model": "x-ai/grok-4-fast:free",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant that detects the ISO 639-1 language code of the following text. Only output the code, nothing else."},
			{"role": "user", "content": text},
		},
		"max_tokens": 8,
	}
	detectBody, _ := json.Marshal(detectPrompt)
	req, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(detectBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var detectResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &detectResp)
	lang = ""
	if len(detectResp.Choices) > 0 {
		lang = strings.TrimSpace(detectResp.Choices[0].Message.Content)
	}
	if lang == "en" {
		return lang, "", nil
	}

	// 2. Translate to English
	translatePrompt := map[string]interface{}{
		"model": "x-ai/grok-4-fast:free",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a translation engine. Translate the following text to English. Only output the translation itself, with no preamble, explanation, or extra text."},
			{"role": "user", "content": text},
		},
		"max_tokens": 2048,
	}
	translateBody, _ := json.Marshal(translatePrompt)
	req2, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(translateBody))
	if err != nil {
		return lang, "", err
	}
	req2.Header.Set("Authorization", "Bearer "+apiKey)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return lang, "", err
	}
	defer resp2.Body.Close()
	var transResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	body2, _ := io.ReadAll(resp2.Body)
	json.Unmarshal(body2, &transResp)
	translation = ""
	if len(transResp.Choices) > 0 {
		translation = strings.TrimSpace(transResp.Choices[0].Message.Content)
	}
	return lang, translation, nil
}

var (
	rateLimitMu  sync.Mutex
	rateLimitMap = make(map[string][]int64) // userID -> timestamps

	transcribedMu     sync.Mutex
	transcribedMsgIDs = make(map[string]struct{}) // messageID -> struct{}
	transcribedOrder  []string                    // maintain order of insertion for eviction
)

// Map Discord audio message ID to Speechmatics job ID
var audioMsgIDToJobID = make(map[string]string)

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
	date := time.Now().Format("2006-01-02_15-04-05")
	logDir := "logs"
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		audioMsgIDToJobID = make(map[string]string) // New: Map Discord audio message ID to Speechmatics job ID
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
	// Ignore messages from the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	// --- Rate limiting: 15 uses per 60 minutes per user ---
	const rateLimitCount = 15
	const rateLimitWindow = 60 * 60 // seconds

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

	// Only respond to !transcribe or !t
	cmd := strings.TrimSpace(m.Content)
	if cmd != "!transcribe" && cmd != "!t" {
		return
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
	// ...existing code...

	if audioMsgID != "" {
		transcribedMu.Lock()
		_, already := transcribedMsgIDs[audioMsgID]
		jobID, jobIDExists := audioMsgIDToJobID[audioMsgID]
		transcribedMu.Unlock()
		if already && jobIDExists {
			transcript, err := fetchSpeechmaticsTranscript(jobID)
			if err != nil {
				s.ChannelMessageSend(m.ChannelID, "Error fetching previous transcript: "+err.Error())
				return
			}
			if strings.TrimSpace(transcript) == "" {
				s.ChannelMessageSend(m.ChannelID, "This audio message has already been transcribed, but the transcript is empty.")
				return
			}
			username := "Someone"
			if targetMsg != nil && targetMsg.Author != nil {
				username = targetMsg.Author.Username
			}
			replyText := username + " said, \"" + transcript + "\""
			parts := splitMessage(replyText, maxDiscordMsgLen)
			ref := &discordgo.MessageReference{
				MessageID: m.ID,
				ChannelID: m.ChannelID,
				GuildID:   m.GuildID,
			}
			if targetMsg != nil {
				ref.MessageID = targetMsg.ID
			}
			var transcriptMsgID string
			for i, part := range parts {
				msgSend := &discordgo.MessageSend{Content: part}
				if i == 0 {
					msgSend.Reference = ref
				}
				sentMsg, errSend := s.ChannelMessageSendComplex(m.ChannelID, msgSend)
				if errSend != nil {
					log.Printf("Failed to send transcript reply: %v", errSend)
					botLogger.Errorf("Failed to send transcript reply for user %s (%s): %v", m.Author.Username, m.Author.ID, errSend)
					break
				}
				if i == 0 && sentMsg != nil {
					transcriptMsgID = sentMsg.ID
				}
			}
			// Translation logic for repeated requests
			var lang, translation string
			var terr error
			lang, translation, terr = detectAndTranslate(transcript)
			if lang != "en" && translation != "" && terr == nil && transcriptMsgID != "" {
				transParts := splitMessage("Translation: "+translation, maxDiscordMsgLen)
				transRef := &discordgo.MessageReference{
					MessageID: transcriptMsgID,
					ChannelID: m.ChannelID,
					GuildID:   m.GuildID,
				}
				for i, part := range transParts {
					msgSend := &discordgo.MessageSend{Content: part}
					if i == 0 {
						msgSend.Reference = transRef
					}
					_, err = s.ChannelMessageSendComplex(m.ChannelID, msgSend)
					if err != nil {
						log.Printf("Failed to send translation: %v", err)
						botLogger.Errorf("Failed to send translation for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
						break
					}
				}
			}
			return
		} else if already {
			s.ChannelMessageSend(m.ChannelID, "This audio message has already been transcribed.")
			return
		}
		// Mark as transcribed
		transcribedMu.Lock()
		transcribedMsgIDs[audioMsgID] = struct{}{}
		transcribedOrder = append(transcribedOrder, audioMsgID)
		// If over 100, evict oldest
		if len(transcribedOrder) > 100 {
			oldest := transcribedOrder[0]
			delete(transcribedMsgIDs, oldest)
			delete(audioMsgIDToJobID, oldest)
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
	var transcript, jobID string
	var transcribeErr error
	func() {
		defer func() {
			if err := os.Remove(tmpFile); err != nil {
				log.Printf("Failed to remove temp file: %v", err)
				botLogger.Errorf("Failed to remove temp file: %v", err)
			}
		}()
		transcript, jobID, transcribeErr = transcribeWithJobID(tmpFile)
	}()

	username := "Someone"
	userID := ""
	if targetMsg != nil && targetMsg.Author != nil {
		username = targetMsg.Author.Username
		userID = targetMsg.Author.ID
	}

	// Log every transcription request, even if blank
	botLogger.Logf("Transcription request: user=%s (%s), targetUser=%s (%s), transcript=%q, jobID=%s", m.Author.Username, m.Author.ID, username, userID, transcript, jobID)

	if transcribeErr != nil {
		log.Printf("Transcription failed: %v", transcribeErr)
		botLogger.Errorf("Transcription failed for user %s (%s): %v", m.Author.Username, m.Author.ID, transcribeErr)
		s.ChannelMessageSend(m.ChannelID, "Error transcribing: "+transcribeErr.Error())
		return
	}

	// Store jobID for future duplicate requests
	if audioMsgID != "" && jobID != "" {
		transcribedMu.Lock()
		audioMsgIDToJobID[audioMsgID] = jobID
		transcribedMu.Unlock()
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

	var lang, translation string
	var terr error
	lang, translation, terr = detectAndTranslate(transcript)
	// ... existing code ...

	replyText := username + " said, \"" + transcript + "\""

	parts := splitMessage(replyText, maxDiscordMsgLen)
	ref := &discordgo.MessageReference{
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		GuildID:   m.GuildID,
	}
	if targetMsg != nil {
		ref.MessageID = targetMsg.ID
	}

	var transcriptMsgID string
	for i, part := range parts {
		msgSend := &discordgo.MessageSend{Content: part}
		if i == 0 {
			msgSend.Reference = ref
		}
		sentMsg, errSend := s.ChannelMessageSendComplex(m.ChannelID, msgSend)
		if errSend != nil {
			log.Printf("Failed to send transcript reply: %v", errSend)
			botLogger.Errorf("Failed to send transcript reply for user %s (%s): %v", m.Author.Username, m.Author.ID, errSend)
			break
		}
		if i == 0 && sentMsg != nil {
			transcriptMsgID = sentMsg.ID
		}
	}

	// If not English and translation succeeded, send translation as a reply to the transcript message
	if lang != "en" && translation != "" && terr == nil && transcriptMsgID != "" {
		transParts := splitMessage("Translation: "+translation, maxDiscordMsgLen)
		transRef := &discordgo.MessageReference{
			MessageID: transcriptMsgID,
			ChannelID: m.ChannelID,
			GuildID:   m.GuildID,
		}
		for i, part := range transParts {
			msgSend := &discordgo.MessageSend{Content: part}
			if i == 0 {
				msgSend.Reference = transRef
			}
			_, err = s.ChannelMessageSendComplex(m.ChannelID, msgSend)
			if err != nil {
				log.Printf("Failed to send translation: %v", err)
				botLogger.Errorf("Failed to send translation for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
				break
			}
		}
	}
}
