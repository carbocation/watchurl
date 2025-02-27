package main

import (
	"bytes"
	"database/sql"
	"embed"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	_ "modernc.org/sqlite"
)

//go:embed templates/*
var templatesFS embed.FS

var (
	// Load the templates from the embedded filesystem.
	indexTmpl   = template.Must(template.ParseFS(templatesFS, "templates/index.html"))
	historyTmpl = template.Must(template.ParseFS(templatesFS, "templates/history.html"))
	diffTmpl    = template.Must(template.ParseFS(templatesFS, "templates/diff.html"))
)

// MonitoredURL represents a URL to be watched. Frequency is stored as a time.Duration (in nanoseconds).
// When a user enters a frequency in seconds, it is converted by multiplying with time.Second.
type MonitoredURL struct {
	ID        int
	URL       string
	Frequency time.Duration
}

// MonitoredURLView is used to pass URL data (with frequency in seconds) to the index template.
type MonitoredURLView struct {
	ID        int
	URL       string
	Frequency int
}

// Snapshot represents a URL snapshot for display.
type Snapshot struct {
	ID        int
	Timestamp string
	Content   template.HTML
}

// DiffSnapshot is a helper struct for displaying diffs in the history view.
type DiffSnapshot struct {
	Snapshot Snapshot
	// NextID holds the id of the next (older) snapshot, if available.
	NextID int
}

// HistoryView contains the URL and its snapshots for the history page.
type HistoryView struct {
	URL       string
	Snapshots []DiffSnapshot
}

var (
	db *sql.DB
	// mu protects concurrent writes to the database.
	mu sync.Mutex
)

func main() {
	var err error
	// Open (or create) the SQLite database file using modernc's pure Go driver.
	db, err = sql.Open("sqlite", "./monitor.db")
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer db.Close()

	// Initialize database tables.
	if err = setupDatabase(); err != nil {
		log.Fatalf("Error setting up database: %v", err)
	}

	// Load monitored URLs from the database and start monitoring.
	rows, err := db.Query("SELECT id, url, frequency FROM monitored_urls")
	if err != nil {
		log.Fatalf("Error querying monitored URLs: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m MonitoredURL
		var freqSeconds int
		if err := rows.Scan(&m.ID, &m.URL, &freqSeconds); err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}
		m.Frequency = time.Duration(freqSeconds) * time.Second
		go monitorURL(m)
	}

	// Setup HTTP handlers.
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/add", addURLHandler)
	http.HandleFunc("/delete", deleteURLHandler)
	http.HandleFunc("/history", historyHandler)
	http.HandleFunc("/diff", diffHandler)

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// setupDatabase creates necessary tables if they don't exist.
func setupDatabase() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS monitored_urls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url TEXT NOT NULL,
			frequency INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS url_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url_id INTEGER NOT NULL,
			timestamp DATETIME NOT NULL,
			content TEXT,
			FOREIGN KEY(url_id) REFERENCES monitored_urls(id)
		);`,
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// monitorURL periodically checks the given URL and saves a snapshot if content changes.
// monitorURL periodically checks the given URL and saves a snapshot if the (filtered) content changes.
func monitorURL(m MonitoredURL) {
	var lastContent string

	// Retrieve the most recent snapshot for this URL, if it exists.
	err := db.QueryRow("SELECT content FROM url_snapshots WHERE url_id = ? ORDER BY timestamp DESC LIMIT 1", m.ID).Scan(&lastContent)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("Error retrieving last snapshot for URL id %d: %v", m.ID, err)
	}

	// Take an initial snapshot immediately.
	log.Printf("Taking initial snapshot for URL: %s", m.URL)
	resp, err := fetchURL(m.URL)
	if err != nil {
		log.Printf("Error fetching %s: %v", m.URL, err)
	} else {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("Error reading response from %s: %v", m.URL, err)
		} else {
			currentContent := string(bodyBytes)
			// Extract <body> content and strip out non-visible tags like <meta>.
			currentContent = extractBody(currentContent)
			if currentContent != lastContent {
				lastContent = currentContent
				saveSnapshot(m.ID, currentContent)
			} else {
				log.Printf("No change detected on initial check for %s", m.URL)
			}
		}
	}

	ticker := time.NewTicker(m.Frequency)
	defer ticker.Stop()

	for {
		<-ticker.C

		log.Printf("Checking URL: %s", m.URL)
		resp, err := fetchURL(m.URL)
		if err != nil {
			log.Printf("Error fetching %s: %v", m.URL, err)
			continue
		}
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("Error reading response from %s: %v", m.URL, err)
			continue
		}

		currentContent := string(bodyBytes)
		// Filter the content: extract the body and remove non-visible tags.
		currentContent = extractBody(currentContent)
		if currentContent != lastContent {
			log.Printf("Change detected for %s", m.URL)
			lastContent = currentContent
			saveSnapshot(m.ID, currentContent)
		}
	}
}

// saveSnapshot persists a snapshot of the URL content.
func saveSnapshot(urlID int, content string) {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("INSERT INTO url_snapshots (url_id, timestamp, content) VALUES (?, ?, ?)",
		urlID, time.Now(), content)
	if err != nil {
		log.Printf("Error saving snapshot for URL id %d: %v", urlID, err)
	}
}

func fetchURL(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Set a common user agent, e.g., mimicking Chrome on Windows.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "+
		"AppleWebKit/537.36 (KHTML, like Gecko) "+
		"Chrome/90.0.4430.93 Safari/537.36")
	return http.DefaultClient.Do(req)
}

// extractBody parses the input HTML and returns only the inner HTML of the <body> tag,
// while stripping out non-visible tags (e.g. <meta>). If no <body> tag is found or the input
// isn’t valid HTML, the original input is returned.
func extractBody(input string) string {
	doc, err := html.Parse(strings.NewReader(input))
	if err != nil {
		// Input is not valid HTML; return the original content.
		return input
	}

	var body *html.Node
	var findBody func(*html.Node)
	findBody = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findBody(c)
			if body != nil {
				return
			}
		}
	}
	findBody(doc)

	if body == nil {
		return input
	}

	// Remove non-visible tags such as <meta> from the <body> node.
	removeMetaNodes(body)

	var buf bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&buf, c); err != nil {
			return input
		}
	}
	return buf.String()
}

// removeMetaNodes traverses the node tree under n and removes any <meta> elements.
func removeMetaNodes(n *html.Node) {
	if n == nil {
		return
	}
	var newChildren []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		// Skip any <meta> tags.
		if c.Type == html.ElementNode && c.Data == "meta" {
			continue
		}
		newChildren = append(newChildren, c)
	}
	// Rebuild the child linked list.
	if len(newChildren) > 0 {
		n.FirstChild = newChildren[0]
		newChildren[0].PrevSibling = nil
		for i := 1; i < len(newChildren); i++ {
			newChildren[i].PrevSibling = newChildren[i-1]
			newChildren[i-1].NextSibling = newChildren[i]
		}
		newChildren[len(newChildren)-1].NextSibling = nil
	} else {
		n.FirstChild = nil
	}
	// Recursively process each remaining child.
	for _, c := range newChildren {
		removeMetaNodes(c)
	}
}
