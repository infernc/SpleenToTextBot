package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

// Helper to split at word boundaries
const maxDiscordMsgLen = 2000

// Resilience config for handling long audio
const (
	maxTranscriptSize = 10 * 1024 * 1024        // 10MB max for transcript JSON (17min audio ~1-2MB)
	maxStatusBodySize = 2 * 1024 * 1024         // 2MB max for status response
	maxUsageBodySize  = 512 * 1024              // 512KB max for usage response
	maxDownloadSize   = 50 * 1024 * 1024        // 50MB max for audio file (safety margin)
	httpTimeout       = 30 * time.Second        // timeout for HTTP requests
	statusPollTimeout = 2 * time.Hour           // max time to wait for job completion
)

// Global HTTP client with timeouts (reuse connection pool)
var httpClient = &http.Client{
	Timeout: httpTimeout,
	Transport: &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	},
}

// getSpeechmaticsUsage fetches usage statistics from Speechmatics API for the current month
func getSpeechmaticsUsage() string {
	apiKey := os.Getenv("SPEECHMATICS_API_KEY")
	if apiKey == "" {
		return "SPEECHMATICS_API_KEY not set in environment"
	}

	// Calculate date range for current month
	now := time.Now()
	year, month, _ := now.Date()

	// First day of current month
	from := time.Date(year, month, 1, 0, 0, 0, 0, now.Location())
	// Last day of current month (or today if we're past the last day)
	lastDay := time.Date(year, month+1, 0, 23, 59, 59, 999999999, now.Location())

	url := fmt.Sprintf("https://asr.api.speechmatics.com/v2/usage?since=%s&until=%s",
		from.Format("2006-01-02"), lastDay.Format("2006-01-02"))

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "Error creating usage request: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "Error fetching usage statistics: " + err.Error()
	}
	defer resp.Body.Close()
	
	// Limit response size
	limitedBody := io.LimitReader(resp.Body, maxUsageBodySize)
	body, err := io.ReadAll(limitedBody)
	if err != nil {
		return "Error reading usage response: " + err.Error()
	}
	// Parse response
	var usageResp struct {
		Summary []struct {
			DurationHrs float64 `json:"duration_hrs"`
			Count       int     `json:"count"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &usageResp); err != nil {
		return "Error parsing usage response: " + err.Error()
	}
	var totalMinutes float64
	var totalJobs int
	if len(usageResp.Summary) > 0 {
		totalMinutes = usageResp.Summary[0].DurationHrs * 60.0
		totalJobs = usageResp.Summary[0].Count
	}
	secondsPortion := (totalMinutes - math.Floor(totalMinutes)) * 60.0
	minutesPortion := int(math.Floor(totalMinutes))

	// Format month name for display
	monthName := from.Format("Jan, 2006")
	return fmt.Sprintf("Usage for %s: %d minutes %.1f seconds\nNumber of requests serviced: %d\nMonthly usage limit: 480 minutes", monthName, minutesPortion, secondsPortion, totalJobs)
}

func splitMessage(s string, maxLen int) []string {
	var result []string
	runes := []rune(s)
	for len(runes) > 0 {
		if len(runes) <= maxLen {
			result = append(result, string(runes))
			break
		}
		// Find best split point before maxLen
		// Priority: 1) \n\n (between blocks)  2) space  3) single \n (but not after -# MM:SS)
		cut := maxLen
		found := false
		
		// First try: find \n\n (between transcript blocks)
		for i := maxLen; i > 1; i-- {
			if runes[i] == '\n' && runes[i-1] == '\n' {
				cut = i + 1
				found = true
				break
			}
		}
		
		// Second try: find space
		if !found {
			for i := maxLen; i > 0; i-- {
				if runes[i] == ' ' {
					cut = i
					found = true
					break
				}
			}
		}
		
		// Third try: find single \n but NOT right after "-# MM:SS"
		if !found {
			for i := maxLen; i > 0; i-- {
				if runes[i] == '\n' {
					// Check if this is right after a timestamp line
					// Look backwards for "-# " pattern
					isAfterTimestamp := false
					for j := i - 1; j >= 0 && j > i-10; j-- {
						if j >= 2 && runes[j] == '#' && runes[j-1] == '-' {
							isAfterTimestamp = true
							break
						}
						if runes[j] == '\n' {
							break
						}
					}
					if !isAfterTimestamp {
						cut = i + 1
						found = true
						break
					}
				}
			}
		}
		
		if !found {
			cut = maxLen // hard cut as last resort
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

// Speechmatics transcript item from JSON response
type TranscriptItem struct {
	Type        string  `json:"type"`
	StartTime   float64 `json:"start_time"`
	EndTime     float64 `json:"end_time"`
	Alternatives []struct {
		Content    string  `json:"content"`
		Confidence float64 `json:"confidence"`
	} `json:"alternatives"`
	// Additional fields that might be in json-v2
	Punctuation bool   `json:"is_punctuation"`
	Speaker     int    `json:"speaker_id"`
}

// Speechmatics JSON transcript response
type TranscriptResponse struct {
	Results []TranscriptItem `json:"results"`
}

// fetchSpeechmaticsTranscriptJSON fetches transcript in JSON format for a given job ID
func fetchSpeechmaticsTranscriptJSON(jobID string) (TranscriptResponse, error) {
	apiKey := os.Getenv("SPEECHMATICS_API_KEY")
	if apiKey == "" {
		return TranscriptResponse{}, fmt.Errorf("SPEECHMATICS_API_KEY not set in environment")
	}
	transcriptURL := "https://asr.api.speechmatics.com/v2/jobs/" + jobID + "/transcript?format=json-v2"
	req, err := http.NewRequest("GET", transcriptURL, nil)
	if err != nil {
		return TranscriptResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// botLogger.Logf("[DEBUG] Fetching transcript for job %s...", jobID)
	resp, err := httpClient.Do(req)
	if err != nil {
		// botLogger.Errorf("[DEBUG] Transcript fetch failed: %v", err)
		return TranscriptResponse{}, fmt.Errorf("transcript fetch timeout/error: %v", err)
	}
	defer resp.Body.Close()
	// botLogger.Logf("[DEBUG] Transcript response status: %d, Content-Length: %d", resp.StatusCode, resp.ContentLength)
	
	// Use LimitedReader to prevent memory bomb from huge transcripts
	limitedBody := io.LimitReader(resp.Body, maxTranscriptSize)
	body, err := io.ReadAll(limitedBody)
	if err != nil {
		// botLogger.Errorf("[DEBUG] Failed to read transcript body: %v", err)
		return TranscriptResponse{}, fmt.Errorf("failed to read transcript body: %v", err)
	}
	// botLogger.Logf("[DEBUG] Successfully read %d bytes from transcript", len(body))
	
	// Check if we hit the size limit (try to read one more byte)
	var testByte [1]byte
	n, _ := resp.Body.Read(testByte[:])
	if n > 0 {
		// botLogger.Errorf("[DEBUG] ERROR: Transcript was truncated - exceeded %dMB limit", maxTranscriptSize/1024/1024)
		return TranscriptResponse{}, fmt.Errorf("transcript too large (>%dMB) - audio may be too long", maxTranscriptSize/1024/1024)
	}
	
	// Check for error response
	if resp.StatusCode != 200 {
		// botLogger.Errorf("[DEBUG] API error (status %d): %s", resp.StatusCode, string(body))
		return TranscriptResponse{}, fmt.Errorf("speechmatics API error (status %d): %s", resp.StatusCode, string(body))
	}
	
	// botLogger.Logf("[DEBUG] Parsing JSON transcript...")
	var transcriptResp TranscriptResponse
	if err := json.Unmarshal(body, &transcriptResp); err != nil {
		// botLogger.Errorf("[DEBUG] JSON unmarshal failed: %v", err)
		return TranscriptResponse{}, fmt.Errorf("failed to parse transcript JSON: %v", err)
	}
	// botLogger.Logf("[DEBUG] Successfully parsed transcript: %d results", len(transcriptResp.Results))
	
	return transcriptResp, nil
}

// formatTranscript formats the JSON transcript into a readable string
// If withTimestamps is true, adds Discord subtext timestamps above each line
func formatTranscript(resp TranscriptResponse, withTimestamps bool) string {
	if len(resp.Results) == 0 {
		// botLogger.Logf("[DEBUG] formatTranscript: NO results in response")
		return ""
	}
	// botLogger.Logf("[DEBUG] formatTranscript: starting with %d results, withTimestamps=%v", len(resp.Results), withTimestamps)
	
	// Collect all words with their timing
	var words []struct {
		Text      string
		StartTime float64
	}
	
	for _, item := range resp.Results {
		// json-v2 format: items can be "word" or "punctuation"
		if len(item.Alternatives) == 0 {
			continue
		}
		
		content := item.Alternatives[0].Content
		
		// Skip standalone punctuation marks (they cause orphan lines like ".")
		// These are already handled by the spacing logic that attaches punctuation to words
		if content == "." || content == "," || content == "!" || content == "?" || 
		   content == ";" || content == ":" || content == "..." || content == "–" ||
		   content == "-" || content == "—" {
			continue
		}
		
		words = append(words, struct {
			Text      string
			StartTime float64
		}{Text: content, StartTime: item.StartTime})
		
		// Log progress every 500 items (commented out for memory efficiency)
		// if (idx+1) % 500 == 0 {
		// 	botLogger.Logf("[DEBUG] formatTranscript progress: %d/%d items, %d words collected", idx+1, len(resp.Results), len(words))
		// }
	}
	
	// botLogger.Logf("[DEBUG] formatTranscript: collected %d words from %d items", len(words), len(resp.Results))
	if len(words) == 0 {
		// botLogger.Logf("[DEBUG] formatTranscript: NO words extracted (all items had empty alternatives)")
		return ""
	}
	
	// Group into lines by sentence boundaries
	var lines []struct {
		Text      string
		StartTime float64
	}
	
	var currentLine strings.Builder
	var lineStartTime float64 = words[0].StartTime
	
	for i, word := range words {
		// Add space before word unless it's punctuation or first word
		if currentLine.Len() > 0 && !strings.HasPrefix(word.Text, ".") && !strings.HasPrefix(word.Text, ",") && 
		   !strings.HasPrefix(word.Text, "!") && !strings.HasPrefix(word.Text, "?") && !strings.HasPrefix(word.Text, ";") &&
		   !strings.HasPrefix(word.Text, ":") {
			currentLine.WriteString(" ")
		}
		currentLine.WriteString(word.Text)
		
		// Check if word ends with sentence-ending punctuation
		isSentenceEnd := strings.HasSuffix(word.Text, ".") || strings.HasSuffix(word.Text, "!") || strings.HasSuffix(word.Text, "?")
		
		// Also split every ~15 words for readability if no punctuation
		isLongLine := currentLine.Len() > 150
		
		if isSentenceEnd || isLongLine || i == len(words)-1 {
			lineText := strings.TrimSpace(currentLine.String())
			if lineText != "" {
				lines = append(lines, struct {
					Text      string
					StartTime float64
				}{Text: lineText, StartTime: lineStartTime})
			}
			currentLine.Reset()
			if i < len(words)-1 {
				lineStartTime = words[i+1].StartTime
			}
		}
	}
	
	// Format output with proper line breaks
	var result strings.Builder
	// botLogger.Logf("[DEBUG] formatTranscript: formatting %d lines into final message", len(lines))
	for i, line := range lines {
		if withTimestamps {
			// Format timestamp as MM:SS
			minutes := int(line.StartTime) / 60
			seconds := int(line.StartTime) % 60
			timestamp := fmt.Sprintf("%02d:%02d", minutes, seconds)
			result.WriteString(fmt.Sprintf("-# %s\n", timestamp))
		}
		result.WriteString(line.Text)
		if i < len(lines)-1 {
			result.WriteString("\n")
		}
	}
	
	formatted := result.String()
	// botLogger.Logf("[DEBUG] formatTranscript: final message size is %d characters", len(formatted))
	return formatted
}

// fetchSpeechmaticsTranscript fetches transcript for a given job ID (legacy, returns plain text)
func fetchSpeechmaticsTranscript(jobID string) (string, error) {
	resp, err := fetchSpeechmaticsTranscriptJSON(jobID)
	if err != nil {
		return "", err
	}
	return formatTranscript(resp, false), nil
}

// transcribeWithJobID returns transcript response and Speechmatics job ID
func transcribeWithJobID(filePath string) (TranscriptResponse, string, error) {
	apiKey := os.Getenv("SPEECHMATICS_API_KEY")
	if apiKey == "" {
		return TranscriptResponse{}, "", fmt.Errorf("SPEECHMATICS_API_KEY not set in environment")
	}

	// Create context with timeout for entire transcription process
	ctx, cancel := context.WithTimeout(context.Background(), statusPollTimeout)
	defer cancel()

	// Open audio file
	audioFile, err := os.Open(filePath)
	if err != nil {
		return TranscriptResponse{}, "", err
	}
	defer audioFile.Close()

	// Prepare multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add config part
	config := `{"type": "transcription", "transcription_config": {"language": "auto", "diarization": "none", "operating_point": "enhanced", "enable_entities": true, "transcript_filtering_config": {"remove_disfluencies": true}}}`
	if err := writer.WriteField("config", config); err != nil {
		return TranscriptResponse{}, "", err
	}

	// Add audio file part (must be named "data_file")
	audioPart, err := writer.CreateFormFile("data_file", filepath.Base(filePath))
	if err != nil {
		return TranscriptResponse{}, "", err
	}
	if _, err := io.Copy(audioPart, audioFile); err != nil {
		return TranscriptResponse{}, "", err
	}

	writer.Close()

	req, err := http.NewRequest("POST", "https://asr.api.speechmatics.com/v2/jobs", &buf)
	if err != nil {
		return TranscriptResponse{}, "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req = req.WithContext(ctx)

	resp, err := httpClient.Do(req)
	if err != nil {
		return TranscriptResponse{}, "", fmt.Errorf("job submission failed: %v", err)
	}
	defer resp.Body.Close()

	limitedBody := io.LimitReader(resp.Body, maxStatusBodySize)
	respBody, err := io.ReadAll(limitedBody)
	if err != nil {
		return TranscriptResponse{}, "", err
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
		return TranscriptResponse{}, "", fmt.Errorf("failed to parse Speechmatics job response: %v", err)
	}
	if jr.Error != "" || jr.Code != 0 {
		// Submission error
		return TranscriptResponse{}, "", fmt.Errorf("speechmatics submission error: %s (%s)", jr.Error, jr.Detail)
	}
	if jr.ID == "" {
		return TranscriptResponse{}, "", fmt.Errorf("no job ID returned from Speechmatics")
	}

	statusURL := "https://asr.api.speechmatics.com/v2/jobs/" + jr.ID
	
	// Exponential backoff polling: 1s, 2s, 4s, 8s, 16s, capped at 30s
	backoff := time.Second
	maxBackoff := 30 * time.Second
	
	for {
		// Respect the global context timeout
		select {
		case <-ctx.Done():
			return TranscriptResponse{}, jr.ID, fmt.Errorf("transcription timeout: job did not complete within %v", statusPollTimeout)
		default:
		}
		
		// Sleep with backoff
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return TranscriptResponse{}, jr.ID, fmt.Errorf("transcription timeout: job did not complete within %v", statusPollTimeout)
		}
		
		req, err := http.NewRequest("GET", statusURL, nil)
		if err != nil {
			return TranscriptResponse{}, "", err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req = req.WithContext(ctx)
		
		pollResp, err := httpClient.Do(req)
		if err != nil {
			// botLogger.Errorf("[DEBUG] Poll attempt failed: %v", err)
			return TranscriptResponse{}, "", fmt.Errorf("status poll failed: %v", err)
		}
		
		limitedBody := io.LimitReader(pollResp.Body, maxStatusBodySize)
		statusBody, err := io.ReadAll(limitedBody)
		pollResp.Body.Close()
		if err != nil {
			return TranscriptResponse{}, "", err
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
		// botLogger.Logf("[DEBUG] Poll #%d - Job status: %s (backoff: %v)", 
		// 	int(statusPollTimeout/(backoff*time.Second)), statusObj.Job.Status, backoff)
		
		switch statusLower {
		case "done":
			// botLogger.Logf("[DEBUG] Job complete! Now fetching transcript...")
			transcriptResp, err := fetchSpeechmaticsTranscriptJSON(jr.ID)
			if err != nil {
				// botLogger.Errorf("[DEBUG] FAILED to fetch transcript: %v", err)
				return TranscriptResponse{}, jr.ID, err
			}
			// botLogger.Logf("[DEBUG] SUCCESS: Transcript fetched and returned")
			return transcriptResp, jr.ID, nil
		case "rejected":
			var errMsg string
			if len(statusObj.Job.Errors) > 0 {
				errMsg = statusObj.Job.Errors[0].Message
			}
			return TranscriptResponse{}, jr.ID, fmt.Errorf("speechmatics job rejected: %s", errMsg)
		case "deleted":
			return TranscriptResponse{}, jr.ID, fmt.Errorf("speechmatics job deleted before completion")
		case "expired":
			return TranscriptResponse{}, jr.ID, fmt.Errorf("speechmatics job expired before completion")
		case "running":
			// Increase backoff for next poll
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		default:
			return TranscriptResponse{}, jr.ID, fmt.Errorf("speechmatics job in unknown state: %s", statusObj.Job.Status)
		}
	}
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
	// Clear logs from memory after saving (prevent memory leak)
	l.logs = nil
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
	
	// Use UUID for unique temp filenames to avoid collisions
	tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", base, time.Now().UnixNano()))

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("Failed to create download request: %v", err)
		return "", err
	}
	
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Failed to download %s: %v", url, err)
		return "", fmt.Errorf("download timeout or failed: %v", err)
	}
	defer resp.Body.Close()

	// Check Content-Length header before downloading
	if resp.ContentLength > maxDownloadSize {
		return "", fmt.Errorf("audio file too large: %d bytes (max %d)", resp.ContentLength, maxDownloadSize)
	}

	out, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("Failed to create temp file: %v", err)
		return "", err
	}
	defer out.Close()

	// Use LimitedReader to prevent downloading oversized files
	limitedBody := io.LimitReader(resp.Body, maxDownloadSize)
	_, err = io.Copy(out, limitedBody)
	if err != nil {
		log.Printf("Failed to save file: %v", err)
		return "", err
	}

	return tmpPath, nil
}

// startTypingLoop starts a cancellable background goroutine that keeps the
// typing indicator active in `channelID` until the returned cancel function
// is called. It triggers an immediate typing event, then repeats every 8s.
func startTypingLoop(s *discordgo.Session, channelID string) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Send an initial typing event immediately
		s.ChannelTyping(channelID)
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.ChannelTyping(channelID)
			}
		}
	}()
	return cancel
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

	dg.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsMessageContent

	dg.StateEnabled = true

	dg.AddHandler(requestMembers)
	dg.AddHandler(messageCreate)

	if err := dg.Open(); err != nil {
		botLogger.Errorf("Error opening Discord session: %v", err)
		botLogger.SaveToFile()
		log.Fatalf("Error opening Discord session: %v", err)
	}
	log.Println("Bot is now running. Press CTRL+C to exit.")
	botLogger.Logf("Bot started and running.")

	// Background goroutine to periodically free memory
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runtime.GC()
			debug.FreeOSMemory()
		}
	}()

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
	// Force garbage collection before exit
	runtime.GC()
}

func requestMembers(s *discordgo.Session, g *discordgo.GuildCreate) {
	log.Printf("Requesting members for guild: %s", g.Name)

	// Request all members for this guild, which forces Discord to send member data.
	err := s.RequestGuildMembers(g.ID, "", 0, "", true)

	if err != nil {
		log.Printf("Error requesting guild members for %s: %v", g.Name, err)
	}
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore messages from the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	// --- Rate limiting: 15 uses per 5 minutes per user ---
	const rateLimitCount = 15
	const rateLimitWindow = 60 * 5 // seconds

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
		
		// Clean up empty entries (users with no recent activity)
		if len(filtered) == 0 && len(times) > 0 {
			delete(rateLimitMap, userID)
		}
		
		return false, 0
	}

	// Respond to !usage command
	cmd := strings.TrimSpace(m.Content)
	if cmd == "!usage" {
		usageMsg := getSpeechmaticsUsage()
		s.ChannelMessageSend(m.ChannelID, usageMsg)
		return
	}

	// Respond to f2c command
	if strings.HasPrefix(cmd, "f2c ") {
		tempStr := strings.TrimSpace(strings.TrimPrefix(cmd, "f2c "))
		tempF, err := strconv.ParseFloat(tempStr, 64)
		if err != nil {
			s.ChannelMessageSend(m.ChannelID, "Invalid temperature value. Please provide a number.")
			return
		}
		tempC := (tempF - 32) * 5 / 9
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%.1f°F is %.1f°C", tempF, tempC))
		return
	}

	// Respond to c2f command
	if strings.HasPrefix(cmd, "c2f ") {
		tempStr := strings.TrimSpace(strings.TrimPrefix(cmd, "c2f "))
		tempC, err := strconv.ParseFloat(tempStr, 64)
		if err != nil {
			s.ChannelMessageSend(m.ChannelID, "Invalid temperature value. Please provide a number.")
			return
		}
		tempF := tempC*9/5 + 32
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("%.1f°C is %.1f°F", tempC, tempF))
		return
	}

	// Respond to !transcribe or !t (with optional -s flag for timestamps)
	showTimestamps := false
	if strings.HasPrefix(cmd, "!transcribe ") || strings.HasPrefix(cmd, "!t ") {
		// Parse flags
		parts := strings.Fields(cmd)
		for _, part := range parts[1:] {
			if part == "-s" {
				showTimestamps = true
			}
		}
	} else if cmd != "!transcribe" && cmd != "!t" {
		return
	}
	// Check rate limit
	limited, mins := isRateLimited(m.Author.ID)
	if limited {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("rate limit exceeded. counter resets in %d minutes", mins))
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

	// If not found in reply, search last 5 messages in channel for the oldest voice note
	if att == nil {
		msgs, err := s.ChannelMessages(m.ChannelID, 5, "", "", "")
		if err == nil {
			// Discord returns messages newest-first (index 0 = newest).
			// Iterate from newest to oldest to find the oldest attachment among the last 5 messages.
			for i := 0; i < len(msgs); i++ {
				msg := msgs[i]
				if msg.ID == m.ID {
					continue
				}
				for _, a := range msg.Attachments {
					name := strings.ToLower(a.Filename)
					if strings.HasSuffix(name, ".ogg") || strings.HasSuffix(name, ".mp3") || strings.HasSuffix(name, ".wav") {
						targetMsg = msg
						att = a
					}
				}
				// Don't break - keep going to find older attachments
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
		jobID, jobIDExists := audioMsgIDToJobID[audioMsgID]
		transcribedMu.Unlock()
		if already && jobIDExists {
			cancelTyping := startTypingLoop(s, m.ChannelID) // keep typing until we reply
			defer cancelTyping()
			transcriptResp, err := fetchSpeechmaticsTranscriptJSON(jobID)
			if err != nil {
				s.ChannelMessageSend(m.ChannelID, "Error fetching previous transcript: "+err.Error())
				return
			}
			transcript := formatTranscript(transcriptResp, showTimestamps)
			// Free memory from large transcript response
			transcriptResp = TranscriptResponse{}
			runtime.GC()
			debug.FreeOSMemory()
			if strings.TrimSpace(transcript) == "" {
				s.ChannelMessageSend(m.ChannelID, "This audio message has already been transcribed, but the transcript is empty.")
				return
			}
			username := "Someone"
			if targetMsg != nil && targetMsg.Author != nil {
				if targetMsg.Member != nil && targetMsg.Member.Nick != "" {
					username = targetMsg.Member.Nick
				} else {
					var member *discordgo.Member
					member, err = s.State.Member(m.GuildID, targetMsg.Author.ID)
					if err == nil && member.Nick != "" {
						username = member.Nick
					} else {
						// Fall back to the global username or global display name
						username = targetMsg.Author.Username
						botLogger.Logf("%v", err)
					}
				}
			}
			// Build reply that references the summon (command) message and
			// include a formatted link to the original audio message when available.
			var replyText string
			if targetMsg != nil {
				audioURL := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", m.GuildID, targetMsg.ChannelID, targetMsg.ID)
				replyText = fmt.Sprintf("## [%s:](%s)\n%s", username, audioURL, transcript)
			} else {
				replyText = fmt.Sprintf("## %s:\n%s", username, transcript)
			}
			parts := splitMessage(replyText, maxDiscordMsgLen)
			// Always reply to the summon (command) message so the response appears
			// as a reply to the user who invoked the bot.
			ref := &discordgo.MessageReference{
				MessageID: m.ID,
				ChannelID: m.ChannelID,
				GuildID:   m.GuildID,
			}
			for i, part := range parts {
				msgSend := &discordgo.MessageSend{Content: part}
				if i == 0 {
					msgSend.Reference = ref
				}
				_, errSend := s.ChannelMessageSendComplex(m.ChannelID, msgSend)
				if errSend != nil {
					log.Printf("Failed to send transcript reply: %v", errSend)
					botLogger.Errorf("Failed to send transcript reply for user %s (%s): %v", m.Author.Username, m.Author.ID, errSend)
					break
				}
			}
			return
		} else if already {
			s.ChannelMessageSend(m.ChannelID, "This audio message has already been rejected.")
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

	cancelTyping := startTypingLoop(s, m.ChannelID) // keep typing until we reply
	defer cancelTyping()
	tmpFile, err := downloadFile(att.URL)
	if err != nil {
		botLogger.Logf("Transcription request: user=%s (%s), targetUser=unknown, transcript=", m.Author.Username, m.Author.ID)
		log.Printf("Download error: %v", err)
		botLogger.Errorf("Download error for user %s (%s): %v", m.Author.Username, m.Author.ID, err)
		s.ChannelMessageSend(m.ChannelID, "Error transcribing: "+err.Error())
		return
	}

	// Always remove the temp file, even if transcription fails
	var transcriptResp TranscriptResponse
	var jobID string
	var transcribeErr error
	func() {
		defer func() {
			if err := os.Remove(tmpFile); err != nil {
				log.Printf("Failed to remove temp file: %v", err)
				botLogger.Errorf("Failed to remove temp file: %v", err)
			}
		}()
		// botLogger.Logf("[DEBUG] Starting transcribeWithJobID for file: %s", tmpFile)
		transcriptResp, jobID, transcribeErr = transcribeWithJobID(tmpFile)
		// botLogger.Logf("[DEBUG] transcribeWithJobID returned: jobID=%s, respItems=%d, err=%v", jobID, len(transcriptResp.Results), transcribeErr)
	}()

	if transcribeErr != nil {
		// botLogger.Errorf("[DEBUG] TRANSCRIPTION ERROR: %v", transcribeErr)
		log.Printf("Transcription failed: %v", transcribeErr)
		botLogger.Errorf("Transcription failed for user %s (%s): %v", m.Author.Username, m.Author.ID, transcribeErr)
		s.ChannelMessageSend(m.ChannelID, "Error transcribing: "+transcribeErr.Error())
		return
	}

	// botLogger.Logf("[DEBUG] Calling formatTranscript with showTimestamps=%v", showTimestamps)
	transcript := formatTranscript(transcriptResp, showTimestamps)
	// botLogger.Logf("[DEBUG] formatTranscript returned %d characters", len(transcript))
	
	// Free memory from large transcript response
	transcriptResp = TranscriptResponse{}
	runtime.GC()
	debug.FreeOSMemory()

	username := "Someone"
	userID := ""
	if targetMsg != nil && targetMsg.Author != nil {
		if targetMsg.Member != nil && targetMsg.Member.Nick != "" {
			username = targetMsg.Member.Nick
		} else {
			var member *discordgo.Member
			member, err = s.State.Member(m.GuildID, targetMsg.Author.ID) //only method that works
			if err == nil && member.Nick != "" {
				username = member.Nick
			} else {
				// Fall back to the global username or global display name
				username = targetMsg.Author.Username
				botLogger.Logf("%v", err)
			}
		}
		userID = targetMsg.Author.ID
	}

	// Log every transcription request, even if blank
	botLogger.Logf("Transcription request: user=%s (%s), targetUser=%s (%s), jobID=%s", m.Author.Username, m.Author.ID, username, userID, jobID)

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

	// Build reply that references the summon (command) message and
	// include a formatted link to the original audio message when available.
	var replyText string
	if targetMsg != nil {
		audioURL := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", m.GuildID, targetMsg.ChannelID, targetMsg.ID)
		replyText = fmt.Sprintf("## [%s:](%s)\n%s", username, audioURL, transcript)
	} else {
		replyText = fmt.Sprintf("## %s:\n%s", username, transcript)
	}
	parts := splitMessage(replyText, maxDiscordMsgLen)
	// Always reply to the summon (command) message so the response appears
	// as a reply to the user who invoked the bot.
	ref := &discordgo.MessageReference{
		MessageID: m.ID,
		ChannelID: m.ChannelID,
		GuildID:   m.GuildID,
	}

	for i, part := range parts {
		msgSend := &discordgo.MessageSend{Content: part}
		if i == 0 {
			msgSend.Reference = ref
		}
		// botLogger.Logf("[DEBUG] Sending message part %d/%d (%d chars)", i+1, len(parts), len(part))
		_, errSend := s.ChannelMessageSendComplex(m.ChannelID, msgSend)
		if errSend != nil {
			log.Printf("Failed to send transcript reply: %v", errSend)
			botLogger.Errorf("Failed to send transcript reply for user %s (%s): %v", m.Author.Username, m.Author.ID, errSend)
			break
		}
		// botLogger.Logf("[DEBUG] Message part %d sent successfully", i+1)
	}
}
