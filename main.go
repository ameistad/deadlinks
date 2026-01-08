package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/time/rate"
)

var verbose bool

type LinkResult struct {
	URL        string
	StatusCode int
	Status     string
	IsInternal bool
	FoundOn    string // The page where this link was found
	Error      error
}

type workItem struct {
	url     string
	foundOn string
}

type Crawler struct {
	startURL    *url.URL
	client      *http.Client
	visited     map[string]bool
	visitedMu   sync.RWMutex
	results     chan LinkResult
	workQueue   chan workItem
	limiter     *rate.Limiter
	ctx         context.Context
	cancel      context.CancelFunc
	workerCount int
	pending     int64
}

func NewCrawler(startURL string, workerCount int) (*Crawler, error) {
	parsed, err := url.Parse(startURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	if parsed.Scheme == "" {
		parsed.Scheme = "https"
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Crawler{
		startURL: parsed,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		visited:     make(map[string]bool),
		results:     make(chan LinkResult, 100),
		workQueue:   make(chan workItem, 10000),
		limiter:     rate.NewLimiter(rate.Every(100*time.Millisecond), 5),
		ctx:         ctx,
		cancel:      cancel,
		workerCount: workerCount,
	}, nil
}

func (c *Crawler) isInternal(link *url.URL) bool {
	return link.Host == c.startURL.Host || link.Host == ""
}

func (c *Crawler) normalizeURL(link string, base *url.URL) (string, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return "", err
	}

	resolved := base.ResolveReference(parsed)
	resolved.Fragment = ""

	if resolved.Path == "" {
		resolved.Path = "/"
	}

	return resolved.String(), nil
}

func (c *Crawler) markVisited(urlStr string) bool {
	c.visitedMu.Lock()
	defer c.visitedMu.Unlock()

	if c.visited[urlStr] {
		return false
	}
	c.visited[urlStr] = true
	return true
}

func (c *Crawler) extractLinks(body *html.Node, baseURL *url.URL) []string {
	var links []string

	var traverse func(*html.Node)
	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					link := strings.TrimSpace(attr.Val)
					if link == "" ||
						strings.HasPrefix(link, "javascript:") ||
						strings.HasPrefix(link, "mailto:") ||
						strings.HasPrefix(link, "tel:") ||
						strings.HasPrefix(link, "#") {
						continue
					}

					normalized, err := c.normalizeURL(link, baseURL)
					if err == nil {
						links = append(links, normalized)
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			traverse(child)
		}
	}

	traverse(body)
	return links
}

func (c *Crawler) addWork(urlStr string, foundOn string) {
	if c.markVisited(urlStr) {
		atomic.AddInt64(&c.pending, 1)
		select {
		case c.workQueue <- workItem{url: urlStr, foundOn: foundOn}:
		case <-c.ctx.Done():
			atomic.AddInt64(&c.pending, -1)
		}
	}
}

func (c *Crawler) finishWork() {
	if atomic.AddInt64(&c.pending, -1) == 0 {
		c.cancel()
	}
}

func (c *Crawler) checkURL(item workItem) {
	if err := c.limiter.Wait(c.ctx); err != nil {
		c.finishWork()
		return
	}

	req, err := http.NewRequestWithContext(c.ctx, "GET", item.url, nil)
	if err != nil {
		c.results <- LinkResult{
			URL:     item.url,
			FoundOn: item.foundOn,
			Error:   err,
			Status:  "ERROR",
		}
		c.finishWork()
		return
	}

	req.Header.Set("User-Agent", "deadlinks/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		c.results <- LinkResult{
			URL:     item.url,
			FoundOn: item.foundOn,
			Error:   err,
			Status:  "ERROR",
		}
		c.finishWork()
		return
	}
	defer resp.Body.Close()

	parsed, _ := url.Parse(item.url)
	isInternal := c.isInternal(parsed)

	result := LinkResult{
		URL:        item.url,
		StatusCode: resp.StatusCode,
		IsInternal: isInternal,
		FoundOn:    item.foundOn,
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.Status = "OK"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		result.Status = "REDIRECT"
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		result.Status = "CLIENT_ERROR"
	case resp.StatusCode >= 500:
		result.Status = "SERVER_ERROR"
	}

	// Only crawl internal HTML pages for more links
	// Must parse body BEFORE sending result since body can only be read once
	var newLinks []string
	if isInternal && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "text/html") {
			doc, err := html.Parse(resp.Body)
			if err == nil {
				newLinks = c.extractLinks(doc, parsed)
			}
		}
	}

	c.results <- result

	// Add discovered links to work queue
	for _, link := range newLinks {
		c.addWork(link, item.url)
	}

	c.finishWork()
}

func (c *Crawler) worker() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case item, ok := <-c.workQueue:
			if !ok {
				return
			}
			c.checkURL(item)
		}
	}
}

func (c *Crawler) Start() {
	// Start result printer in background
	done := make(chan struct{})
	go func() {
		c.printResults()
		close(done)
	}()

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < c.workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker()
		}()
	}

	// Seed with start URL
	c.addWork(c.startURL.String(), "(start)")

	// Wait for workers to finish
	wg.Wait()
	close(c.results)
	<-done
}

func (c *Crawler) Stop() {
	c.cancel()
}

type pageStats struct {
	ok     int
	errors []string
}

func (c *Crawler) printResults() {
	stats := struct {
		total         int
		ok            int
		redirects     int
		errors        int
		internalPages int
		externalLinks int
	}{}

	// Track internal pages crawled (by their URL)
	internalPagesCrawled := make(map[string]bool)
	var internalPagesOrder []string

	// Track errors found on each page (grouped by FoundOn)
	pageErrors := make(map[string][]string)

	for result := range c.results {
		stats.total++

		linkType := "EXTERNAL"
		if result.IsInternal {
			linkType = "INTERNAL"
			stats.internalPages++
			// Track this internal page
			if !internalPagesCrawled[result.URL] {
				internalPagesCrawled[result.URL] = true
				internalPagesOrder = append(internalPagesOrder, result.URL)
			}
		} else {
			stats.externalLinks++
		}

		isError := result.Status != "OK" && result.Status != "REDIRECT"

		// Track global stats
		switch result.Status {
		case "OK":
			stats.ok++
		case "REDIRECT":
			stats.redirects++
		default:
			stats.errors++
		}

		// Build error line if needed
		if isError {
			var line string
			if result.Status == "ERROR" {
				line = fmt.Sprintf("  [%s] %s - %s (error: %v)\n", linkType, result.Status, result.URL, result.Error)
			} else {
				line = fmt.Sprintf("  [%s] %d %s - %s\n", linkType, result.StatusCode, result.Status, result.URL)
			}
			pageErrors[result.FoundOn] = append(pageErrors[result.FoundOn], line)
		}

		// In verbose mode, print immediately
		if verbose {
			var line string
			if result.Status == "ERROR" {
				line = fmt.Sprintf("  [%s] %s - %s (error: %v)\n", linkType, result.Status, result.URL, result.Error)
			} else {
				line = fmt.Sprintf("  [%s] %d %s - %s\n", linkType, result.StatusCode, result.Status, result.URL)
			}
			fmt.Printf("%s: %s", result.FoundOn, line)
		}
	}

	// Print results (non-verbose mode)
	if !verbose {
		// Print all internal pages crawled
		fmt.Printf("\nPages crawled:\n")
		for _, page := range internalPagesOrder {
			errors := pageErrors[page]
			if len(errors) > 0 {
				fmt.Printf("  %s (%d errors)\n", page, len(errors))
			} else {
				fmt.Printf("  %s\n", page)
			}
		}

		// Print errors grouped by where they were found
		if stats.errors > 0 {
			fmt.Printf("\nErrors found:\n")
			for page, errors := range pageErrors {
				fmt.Printf("\n%s\n", page)
				for _, errLine := range errors {
					fmt.Printf("%s", errLine)
				}
			}
		}
	}

	fmt.Printf("\n--- Summary ---\n")
	fmt.Printf("Internal pages crawled: %d\n", stats.internalPages)
	fmt.Printf("External links checked: %d\n", stats.externalLinks)
	fmt.Printf("Total: %d (OK: %d, Redirects: %d, Errors: %d)\n", stats.total, stats.ok, stats.redirects, stats.errors)
}

func main() {
	flag.BoolVar(&verbose, "v", false, "Show all links (by default only errors are shown)")
	flag.BoolVar(&verbose, "verbose", false, "Show all links (by default only errors are shown)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <url>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExample:\n")
		fmt.Fprintf(os.Stderr, "  %s https://example.com\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -v https://example.com\n", os.Args[0])
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	targetURL := flag.Arg(0)

	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		targetURL = "https://" + targetURL
	}

	crawler, err := NewCrawler(targetURL, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	go func() {
		<-sigChan
		fmt.Println("\n\nStopping crawler...")
		crawler.Stop()
	}()

	fmt.Printf("Starting link checker for: %s\n", targetURL)
	fmt.Printf("Press Ctrl+C to stop\n")

	crawler.Start()
}
