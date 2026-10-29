package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/stealth"
)

// Six Degrees of Letterboxd

var browser *rod.Browser

func initBrowser() {
	fmt.Println("Launching a headless browser... ")
	path, _ := launcher.LookPath()
	u := launcher.New().Bin(path).
		Headless(true).
		MustLaunch()
	browser = rod.New().ControlURL(u).MustConnect()
	fmt.Println("Browser ready!")
}

type SearchRequest struct {
	FromUrl string `json:"from_url"`
	ToUrl   string `json:"to_url"`
}

func main() {
	initBrowser()
	defer browser.MustClose()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.HandleFunc("/search", SixDegreeSearch)
	fmt.Println("six degrees of letterboxd running on http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}

func SixDegreeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "ONLY POST ALLOWED", http.StatusMethodNotAllowed)
		return
	}

	var req SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "INVALID REQUEST", http.StatusBadRequest)
		return
	}
	req.FromUrl = strings.TrimSpace(req.FromUrl)
	req.ToUrl = strings.TrimSpace(req.ToUrl)

	if req.FromUrl == "" || req.ToUrl == "" {
		http.Error(w, "Both from_url and to_url are required", http.StatusBadRequest)
		return
	}

	path, err := BFS(req.FromUrl, req.ToUrl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message": "ok",
		"path":    path,
	})
}

func BFS(from string, to string) (string, error) {
	if foundUrl(from, to) {
		return from, nil
	}

	parent := make(map[string]string)
	parent[from] = "START"

	jobs := make(chan string, 50000)
	var taskWg sync.WaitGroup
	var mu sync.Mutex
	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var foundNeighbor string
	const numWorkers = 18

	// Enqueue initial start URL
	taskWg.Add(1)
	jobs <- from

	// Auto-cancel context when all active work items are finished
	go func() {
		taskWg.Wait()
		cancel()
	}()

	// Spawn worker pool
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case url := <-jobs:
					fmt.Printf("Scanning: %s\n", url)
					neighbors := GetNeighbors(url)

					mu.Lock()
					for _, neighbor := range neighbors {
						if _, exists := parent[neighbor]; !exists {
							parent[neighbor] = url

							if foundUrl(to, neighbor) {
								foundNeighbor = neighbor
								cancel()
								mu.Unlock()
								taskWg.Done()
								return
							}

							taskWg.Add(1)
							select {
							case jobs <- neighbor:
							case <-ctx.Done():
							}
						}
					}
					mu.Unlock()
					taskWg.Done()
				}
			}
		}()
	}

	wg.Wait()

	if foundNeighbor != "" {
		fmt.Printf("MATCH FOUND: %s\n", foundNeighbor)
		return createPath(parent, foundNeighbor)
	}

	return "", fmt.Errorf("no path found")
}

func cleanUrl(u string) string {
	u = strings.TrimSpace(strings.ToLower(u))
	if idx := strings.Index(u, "?"); idx != -1 {
		u = u[:idx]
	}
	if idx := strings.Index(u, "#"); idx != -1 {
		u = u[:idx]
	}
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "www.")
	u = strings.TrimRight(u, "/")
	return u
}

func isValidLetterboxdLink(link string) bool {
	if !strings.HasPrefix(link, "https://letterboxd.com/") {
		return false
	}

	// Must start directly with the core entities (no usernames or reviews in between)
	isCanonical := strings.HasPrefix(link, "https://letterboxd.com/film/") ||
		strings.HasPrefix(link, "https://letterboxd.com/actor/") ||
		strings.HasPrefix(link, "https://letterboxd.com/director/")

	if !isCanonical {
		return false
	}

	// Exclude noisy sub-pages
	badSubpages := []string{"/reviews/", "/lists/", "/ratings/", "/fans/", "/crew/", "/details/", "/contribute/"}
	for _, sub := range badSubpages {
		if strings.Contains(link, sub) {
			return false
		}
	}

	return !strings.Contains(link, "?") && !strings.Contains(link, "#")
}


func foundUrl(a, b string) bool {
	return cleanUrl(a) == cleanUrl(b)
}

func GetNeighbors(url string) []string {
	page, err := stealth.Page(browser)
	if err != nil {
		return []string{}
	}
	defer page.MustClose()

	err = page.Timeout(15 * time.Second).Navigate(url)
	if err != nil {
		return []string{}
	}

	_ = page.Timeout(5 * time.Second).WaitLoad()

	res, err := page.Eval(`() => Array.from(document.querySelectorAll('a')).map(a => a.href)`)
	if err != nil {
		return []string{}
	}

	var links []string
	for _, item := range res.Value.Arr() {
		href := item.Str()

		fullURL := href
		if !strings.HasSuffix(fullURL, "/") && !strings.Contains(fullURL, "?") && !strings.Contains(fullURL, "#") {
			fullURL += "/"
		}

		if isValidLetterboxdLink(fullURL) {
			links = append(links, fullURL)
		}
	}

	return links
}

func createPath(parent map[string]string, neighbor string) (string, error) {
	var steps []string
	curr := neighbor

	for curr != "START" && curr != "" {
		steps = append(steps, curr)
		curr = parent[curr]
	}

	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}

	return strings.Join(steps, " -> "), nil
}
