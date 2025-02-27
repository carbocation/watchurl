package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/joho/godotenv"
)

func init() {
	// Load environment variables from the .env file if it exists.
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables")
	}
}

const (
	pushoverAPIEndpoint = "https://api.pushover.net/1/messages.json"
)

func sendPushoverNotification(monitoredURL string, changeTime time.Time) {
	// Read API keys from environment variables
	pushoverUserKey := os.Getenv("PUSHOVER_USER_KEY")
	pushoverAPIToken := os.Getenv("PUSHOVER_API_TOKEN")

	// Validate that keys are set
	if pushoverUserKey == "" || pushoverAPIToken == "" {
		log.Println("Missing Pushover API key or user key")
		return
	}

	message := fmt.Sprintf("Change detected on %s at %s", monitoredURL, changeTime.Format(time.RFC1123))
	data := url.Values{}
	data.Set("token", pushoverAPIToken)
	data.Set("user", pushoverUserKey)
	data.Set("message", message)
	data.Set("title", "URL Change Notification")
	data.Set("url", monitoredURL)
	data.Set("url_title", "View URL")

	resp, err := http.PostForm(pushoverAPIEndpoint, data)
	if err != nil {
		log.Printf("Error sending Pushover notification: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("Pushover notification sent, status: %s", resp.Status)
}
