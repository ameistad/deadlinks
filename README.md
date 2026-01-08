# deadlinks

A fast, concurrent dead link checker CLI tool written in Go. Crawls websites, finds all links, and reports broken links, redirects, and errors.

## Installation

```bash
# Clone and build
git clone <repo-url>
cd deadlinks
go build -o deadlinks .

# Or install directly
go install github.com/<user>/deadlinks@latest
```

## Usage

```bash
deadlinks [options] <url>
```

### Options

| Flag | Description |
|------|-------------|
| `-v`, `--verbose` | Show all links as they're checked (by default only errors are shown) |

### Commands

```bash
# Check a website for dead links
deadlinks https://example.com

# Check with verbose output (shows all links)
deadlinks -v https://example.com

# HTTP scheme is added automatically if missing
deadlinks example.com

# Check a local development server
deadlinks http://localhost:3000

# Stop anytime with Ctrl+C (shows summary of progress)
```

## Output

### Default Output

Shows all crawled internal pages, then any errors found:

```
Pages crawled:
  https://example.com/
  https://example.com/about
  https://example.com/docs
  https://example.com/contact

Errors found:

https://example.com/about
  [EXTERNAL] 404 CLIENT_ERROR - https://old-partner.com/page
  [EXTERNAL] ERROR - https://dead-site.com (error: connection refused)

--- Summary ---
Internal pages crawled: 4
External links checked: 12
Total: 16 (OK: 14, Redirects: 0, Errors: 2)
```

### Verbose Output (`-v`)

Shows all links as they're checked in real-time:

```
https://example.com:   [INTERNAL] 200 OK - https://example.com/about
https://example.com:   [EXTERNAL] 301 REDIRECT - https://twitter.com/example
https://example.com/about:   [EXTERNAL] 404 CLIENT_ERROR - https://old-partner.com/page
```

### Link Types

| Type | Description |
|------|-------------|
| `INTERNAL` | Same domain as the starting URL (crawled for more links) |
| `EXTERNAL` | Different domain (checked but not crawled) |

### Status Labels

| Label | HTTP Codes | Description |
|-------|------------|-------------|
| `OK` | 2xx | Successful response |
| `REDIRECT` | 3xx | Redirect (301, 302, 307, 308) |
| `CLIENT_ERROR` | 4xx | Client errors (404 Not Found, 403 Forbidden, etc.) |
| `SERVER_ERROR` | 5xx | Server errors (500, 502, 503, etc.) |
| `ERROR` | N/A | Connection failed, timeout, DNS error, etc. |

## Behavior

- **Crawling**: Only internal (same-domain) HTML pages are crawled for additional links
- **External links**: Checked for status but not crawled further
- **Deduplication**: Each unique URL is only checked once
- **Concurrency**: 10 parallel workers
- **Rate limiting**: 10 requests/second with burst of 5 to avoid overwhelming servers
- **Timeout**: 10 seconds per request
- **Redirects**: Reported but not followed (to detect redirect chains)
- **Skipped links**: `javascript:`, `mailto:`, `tel:`, and fragment-only (`#`) links are ignored

## Requirements

- Go 1.21 or later

## Dependencies

- `golang.org/x/net/html` - HTML parsing
- `golang.org/x/time/rate` - Rate limiting

## License

MIT
