package main

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// indexHandler renders the index page using the index template.
func indexHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
        SELECT mu.id, mu.url, mu.frequency, s.last_updated, mu.push_enabled
        FROM monitored_urls mu
        LEFT JOIN (
            SELECT url_id, MAX(timestamp) as last_updated
            FROM url_snapshots
            GROUP BY url_id
        ) s ON mu.id = s.url_id`)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var urls []MonitoredURLView
	for rows.Next() {
		var u MonitoredURLView
		var lastUpdatedStr sql.NullString
		var freqSeconds, pushInt int
		err := rows.Scan(&u.ID, &u.URL, &freqSeconds, &lastUpdatedStr, &pushInt)
		if err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}
		u.Frequency = freqSeconds
		u.PushEnabled = pushInt != 0
		if lastUpdatedStr.Valid {
			// Split the string at the " m=" portion to remove the monotonic clock info.
			cleanTimeStr := strings.Split(lastUpdatedStr.String, " m=")[0]
			// Use a layout that matches the cleaned string.
			parsed, err := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", cleanTimeStr)
			if err != nil {
				u.LastUpdated = lastUpdatedStr.String
			} else {
				u.LastUpdated = humanize.Time(parsed)
			}
		} else {
			u.LastUpdated = "Never"
		}
		urls = append(urls, u)
	}

	if err := indexTmpl.Execute(w, urls); err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
	}
}

// addURLHandler adds a new URL to monitor and starts a goroutine for it.
func addURLHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	urlStr := r.FormValue("url")
	freqStr := r.FormValue("frequency")
	freq, err := strconv.Atoi(freqStr)
	if err != nil || freq <= 0 {
		http.Error(w, "Invalid frequency", http.StatusBadRequest)
		return
	}

	// Read the push notifications setting.
	pushVal := 0
	if r.FormValue("push") != "" {
		pushVal = 1
	}

	// Serialize this write using the same mutex.
	mu.Lock()
	res, err := db.Exec("INSERT INTO monitored_urls (url, frequency, push_enabled) VALUES (?, ?, ?)", urlStr, freq, pushVal)
	mu.Unlock()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	id, err := res.LastInsertId()
	if err == nil {
		m := MonitoredURL{
			ID:          int(id),
			URL:         urlStr,
			Frequency:   time.Duration(freq) * time.Second,
			PushEnabled: pushVal == 1,
		}
		go monitorURL(m)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// deleteURLHandler removes a monitored URL and its snapshots.
func deleteURLHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	mu.Lock()
	_, err = db.Exec("DELETE FROM monitored_urls WHERE id = ?", id)
	if err != nil {
		mu.Unlock()
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	_, err = db.Exec("DELETE FROM url_snapshots WHERE url_id = ?", id)
	mu.Unlock()
	if err != nil {
		log.Printf("Error deleting snapshots for URL id %d: %v", id, err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// historyHandler renders the history page using the history template.
func historyHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	var urlStr string
	err = db.QueryRow("SELECT url FROM monitored_urls WHERE id = ?", id).Scan(&urlStr)
	if err != nil {
		http.Error(w, "URL not found", http.StatusNotFound)
		return
	}

	// Updated query to fetch id, timestamp, and content.
	rows, err := db.Query("SELECT id, timestamp, content FROM url_snapshots WHERE url_id = ? ORDER BY timestamp DESC", id)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var snapshots []Snapshot
	for rows.Next() {
		var snap Snapshot
		var ts time.Time
		var content string // use a temporary string variable
		if err := rows.Scan(&snap.ID, &ts, &content); err != nil {
			continue
		}
		snap.Timestamp = ts.Format(time.RFC1123)
		// Mark the content as trusted HTML.
		snap.Content = template.HTML(content)
		snapshots = append(snapshots, snap)
	}

	// Build DiffSnapshot list: each snapshot (except the last) gets a link to diff with the next snapshot.
	var diffSnaps []DiffSnapshot
	for i, snap := range snapshots {
		ds := DiffSnapshot{Snapshot: snap}
		if i < len(snapshots)-1 {
			ds.NextID = snapshots[i+1].ID
		}
		diffSnaps = append(diffSnaps, ds)
	}

	w.Header().Set("Content-Type", "text/html")
	hv := HistoryView{
		URL:       urlStr,
		Snapshots: diffSnaps,
	}
	if err := historyTmpl.Execute(w, hv); err != nil {
		log.Printf("Template execution error: %v", err)
	}
}

// diffHandler shows a git-like diff between two snapshot versions.
func diffHandler(w http.ResponseWriter, r *http.Request) {
	id1Str := r.URL.Query().Get("id1")
	id2Str := r.URL.Query().Get("id2")
	id1, err := strconv.Atoi(id1Str)
	if err != nil {
		http.Error(w, "Invalid id1", http.StatusBadRequest)
		return
	}
	id2, err := strconv.Atoi(id2Str)
	if err != nil {
		http.Error(w, "Invalid id2", http.StatusBadRequest)
		return
	}

	var content1, content2 string
	err = db.QueryRow("SELECT content FROM url_snapshots WHERE id = ?", id1).Scan(&content1)
	if err != nil {
		http.Error(w, "Snapshot id1 not found", http.StatusNotFound)
		return
	}
	err = db.QueryRow("SELECT content FROM url_snapshots WHERE id = ?", id2).Scan(&content2)
	if err != nil {
		http.Error(w, "Snapshot id2 not found", http.StatusNotFound)
		return
	}

	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(content1, content2, true)
	dmp.DiffCleanupSemantic(diffs)
	diffHTML := dmp.DiffPrettyHtml(diffs)

	// Convert the diffHTML string to template.HTML so it won't be escaped.
	data := struct {
		ID1      int
		ID2      int
		DiffHTML template.HTML
	}{
		ID1:      id1,
		ID2:      id2,
		DiffHTML: template.HTML(diffHTML),
	}

	w.Header().Set("Content-Type", "text/html")
	if err := diffTmpl.Execute(w, data); err != nil {
		log.Printf("Template execution error: %v", err)
	}
}

func togglePushHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	var current int
	err = db.QueryRow("SELECT push_enabled FROM monitored_urls WHERE id = ?", id).Scan(&current)
	if err != nil {
		http.Error(w, "URL not found", http.StatusNotFound)
		return
	}

	// Toggle: if current is 1, switch to 0; otherwise switch to 1.
	newVal := 1
	if current != 0 {
		newVal = 0
	}

	mu.Lock()
	_, err = db.Exec("UPDATE monitored_urls SET push_enabled = ? WHERE id = ?", newVal, id)
	mu.Unlock()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}
