package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// GitHub models (trimmed)
type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Labels      []ghLabel `json:"labels"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
	State       string    `json:"state"`
}

type ghMilestone struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// Jira models (trimmed)
type jiraIssueCreateRequest struct {
	Fields jiraIssueFields `json:"fields"`
}

type jiraIssueFields struct {
	Project   jiraProjectRef   `json:"project"`
	Summary   string           `json:"summary"`
	IssueType jiraIssueTypeRef `json:"issuetype"`
	Labels    []string         `json:"labels,omitempty"`
	// Atlassian Document Format for description
	Description *jiraADFDoc `json:"description,omitempty"`
}

type jiraProjectRef struct {
	ID  string `json:"id,omitempty"`
	Key string `json:"key,omitempty"`
}

type jiraIssueTypeRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ADF (very minimal)
type jiraADFDoc struct {
	Type    string        `json:"type"`
	Version int           `json:"version"`
	Content []jiraADFNode `json:"content"`
}

type jiraADFNode struct {
	Type    string         `json:"type"`
	Content []jiraADFNode  `json:"content,omitempty"`
	Text    string         `json:"text,omitempty"`
	Marks   []jiraADFMark  `json:"marks,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

type jiraADFMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

type jiraCreateResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type jiraSearchResponse struct {
	Total  int              `json:"total"`
	Issues []jiraBasicIssue `json:"issues"`
}

type jiraBasicIssue struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type jiraJQLBatchRequest struct {
	Queries []jiraJQLQuery `json:"queries"`
}

type jiraJQLQuery struct {
	JQL        string   `json:"jql"`
	StartAt    int      `json:"startAt,omitempty"`
	MaxResults int      `json:"maxResults,omitempty"`
	Fields     []string `json:"fields,omitempty"`
}

type jiraJQLBatchResponse struct {
	Results []jiraSearchResponse `json:"results"`
}

type jiraRemoteLinkRequest struct {
	Object jiraRemoteLinkObject `json:"object"`
}

type jiraRemoteLinkObject struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx := context.Background()
	issues, err := fetchGitHubIssues(ctx, cfg)
	if err != nil {
		log.Fatalf("github fetch error: %v", err)
	}

	log.Printf("found %d issues in milestone %q for %s/%s (state=all)", len(issues), cfg.GitHubMilestone, cfg.GitHubOwner, cfg.GitHubRepo)

	for _, is := range issues {
		existsKey, err := jiraFindExisting(ctx, cfg, is.Number)
		if err != nil {
			log.Printf("warn: jira search failed for #%d: %v", is.Number, err)
		}
		if existsKey != "" {
			log.Printf("skip: Jira issue already exists for #%d: %s", is.Number, existsKey)
			continue
		}

		if cfg.DryRun {
			log.Printf("[dry-run] would create/search Jira for #%d %q", is.Number, is.Title)
			continue
		}
		key, err := jiraCreateFromGitHubIssue(ctx, cfg, is)
		if err != nil {
			log.Printf("error: creating Jira for #%d failed: %v", is.Number, err)
			continue
		}
		log.Printf("created Jira issue %s for GitHub #%d", key, is.Number)

		if err := jiraAddRemoteLink(ctx, cfg, key, is.HTMLURL, is.Title); err != nil {
			log.Printf("warn: adding remote link to %s failed: %v", key, err)
		}
	}
	// done
}

// Config
type config struct {
	GitHubToken     string
	GitHubOwner     string
	GitHubRepo      string
	GitHubMilestone string

	JiraBaseURL         string
	JiraEmail           string
	JiraAPIToken        string
	JiraProjectKey      string
	JiraProjectID       string
	JiraIssueType       string
	JiraIssueTypeID     string
	JiraSkipDescription bool

	DryRun      bool
	HTTPTimeout time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		GitHubToken:         os.Getenv("GITHUB_TOKEN"),
		GitHubOwner:         getenvDefault("GITHUB_OWNER", "cert-manager"),
		GitHubRepo:          getenvDefault("GITHUB_REPO", "cert-manager"),
		GitHubMilestone:     getenvDefault("GITHUB_MILESTONE", "1.19"),
		JiraBaseURL:         os.Getenv("JIRA_BASE_URL"),
		JiraEmail:           os.Getenv("JIRA_EMAIL"),
		JiraAPIToken:        os.Getenv("JIRA_API_TOKEN"),
		JiraProjectKey:      os.Getenv("JIRA_PROJECT_KEY"),
		JiraProjectID:       os.Getenv("JIRA_PROJECT_ID"),
		JiraIssueType:       getenvDefault("JIRA_ISSUE_TYPE", "Task"),
		JiraIssueTypeID:     os.Getenv("JIRA_ISSUE_TYPE_ID"),
		JiraSkipDescription: strings.EqualFold(os.Getenv("JIRA_SKIP_DESCRIPTION"), "true"),
		DryRun:              strings.EqualFold(os.Getenv("DRY_RUN"), "true"),
		HTTPTimeout:         20 * time.Second,
	}

	if cfg.GitHubToken == "" {
		return cfg, errors.New("missing GITHUB_TOKEN")
	}
	if cfg.JiraBaseURL == "" || cfg.JiraEmail == "" || cfg.JiraAPIToken == "" || cfg.JiraProjectKey == "" {
		return cfg, errors.New("missing one or more Jira envs: JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN, JIRA_PROJECT_KEY")
	}
	return cfg, nil
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// GitHub client
func fetchGitHubIssues(ctx context.Context, cfg config) ([]ghIssue, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	var all []ghIssue
	perPage := 100
	page := 1
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", url.PathEscape(cfg.GitHubOwner), url.PathEscape(cfg.GitHubRepo))
	// resolve milestone number by title
	msNum, err := fetchMilestoneNumber(ctx, cfg, cfg.GitHubMilestone)
	if err != nil {
		return nil, err
	}
	for {
		reqURL := fmt.Sprintf("%s?state=all&milestone=%d&per_page=%d&page=%d", base, msNum, perPage, page)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "gh-to-jira-bot")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("github: status %d: %s", resp.StatusCode, string(b))
		}
		var pageItems []ghIssue
		if err := json.NewDecoder(resp.Body).Decode(&pageItems); err != nil {
			return nil, err
		}

		all = append(all, pageItems...)

		// Pagination: stop when less than perPage
		if len(pageItems) < perPage {
			break
		}
		page++
	}

	return all, nil
}

// fetchMilestoneNumber returns the milestone number for a given title (state=all)
func fetchMilestoneNumber(ctx context.Context, cfg config, title string) (int, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	perPage := 100
	page := 1
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/milestones", url.PathEscape(cfg.GitHubOwner), url.PathEscape(cfg.GitHubRepo))
	for {
		reqURL := fmt.Sprintf("%s?state=all&per_page=%d&page=%d", base, perPage, page)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "gh-to-jira-bot")
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return 0, fmt.Errorf("github milestones status %d: %s", resp.StatusCode, string(b))
		}
		var items []ghMilestone
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			return 0, err
		}
		for _, m := range items {
			if m.Title == title {
				return m.Number, nil
			}
		}
		if len(items) < perPage {
			break
		}
		page++
	}
	return 0, fmt.Errorf("milestone %q not found", title)
}

// Jira helper: basic auth header
func jiraAuthHeader(email, token string) string {
	raw := email + ":" + token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func jiraFindExisting(ctx context.Context, cfg config, ghNumber int) (string, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	token := fmt.Sprintf("cert-manager#%d", ghNumber)
	jql := fmt.Sprintf(`project = %s AND summary ~ "%s"`, cfg.JiraProjectKey, token)
	// Preferred per docs: GET /rest/api/3/search/jql
	reqURL := fmt.Sprintf("%s/rest/api/3/search/jql?jql=%s&maxResults=2&fields=id,key", strings.TrimRight(cfg.JiraBaseURL, "/"), url.QueryEscape(jql))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err == nil && resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			var out jiraSearchResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err == nil {
				if len(out.Issues) > 0 {
					return out.Issues[0].Key, nil
				}
				return "", nil
			}
		}
		// If removed/unsupported, try POST batch next
	}

	// Fallback: POST /rest/api/3/search/jql (batch)
	payload := jiraJQLBatchRequest{Queries: []jiraJQLQuery{{
		JQL:        jql,
		StartAt:    0,
		MaxResults: 2,
		Fields:     []string{"id", "key"},
	}}}
	b, _ := json.Marshal(payload)
	reqURL = fmt.Sprintf("%s/rest/api/3/search/jql", strings.TrimRight(cfg.JiraBaseURL, "/"))
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bdy, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("jira search (jql batch) status %d: %s", resp.StatusCode, string(bdy))
	}
	var outBatch jiraJQLBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&outBatch); err != nil {
		return "", err
	}
	if len(outBatch.Results) > 0 && len(outBatch.Results[0].Issues) > 0 {
		return outBatch.Results[0].Issues[0].Key, nil
	}
	return "", nil
}

func jiraCreateFromGitHubIssue(ctx context.Context, cfg config, is ghIssue) (string, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	summary := fmt.Sprintf("cert-manager#%d: %s", is.Number, truncate(is.Title, 200))

	labels := []string{"github", "cert-manager", fmt.Sprintf("milestone:%s", cfg.GitHubMilestone)}
	// Deduplicate labels (simple)
	labels = uniqueStrings(labels)

	var desc *jiraADFDoc
	if !cfg.JiraSkipDescription {
		desc = buildJiraADFDescription(is)
	}

	projRef := jiraProjectRef{Key: cfg.JiraProjectKey}
	if cfg.JiraProjectID != "" {
		projRef = jiraProjectRef{ID: cfg.JiraProjectID}
	}
	typeRef := jiraIssueTypeRef{Name: cfg.JiraIssueType}
	if cfg.JiraIssueTypeID != "" {
		typeRef = jiraIssueTypeRef{ID: cfg.JiraIssueTypeID}
	}

	payload := jiraIssueCreateRequest{Fields: jiraIssueFields{
		Project:     projRef,
		Summary:     summary,
		IssueType:   typeRef,
		Labels:      labels,
		Description: desc,
	}}

	b, _ := json.Marshal(payload)
	reqURL := fmt.Sprintf("%s/rest/api/3/issue", strings.TrimRight(cfg.JiraBaseURL, "/"))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if resp.StatusCode == 400 && !cfg.JiraSkipDescription && desc != nil {
			// Retry once without description in case ADF or required fields cause INVALID_INPUT
			payload.Fields.Description = nil
			b2, _ := json.Marshal(payload)
			req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(b2)))
			req2.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
			req2.Header.Set("Accept", "application/json")
			req2.Header.Set("Content-Type", "application/json")
			resp2, err2 := client.Do(req2)
			if err2 != nil {
				return "", fmt.Errorf("jira create retry failed: %v (first: %s)", err2, string(body))
			}
			defer resp2.Body.Close()
			if resp2.StatusCode == 201 {
				var out jiraCreateResponse
				if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
					return "", err
				}
				return out.Key, nil
			}
			body2, _ := io.ReadAll(io.LimitReader(resp2.Body, 8192))
			return "", fmt.Errorf("jira create status %d: %s | retry without description status %d: %s", resp.StatusCode, string(body), resp2.StatusCode, string(body2))
		}
		return "", fmt.Errorf("jira create status %d: %s", resp.StatusCode, string(body))
	}
	var out jiraCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Key, nil
}

func jiraAddRemoteLink(ctx context.Context, cfg config, issueKey, urlStr, title string) error {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	payload := jiraRemoteLinkRequest{Object: jiraRemoteLinkObject{URL: urlStr, Title: title}}
	b, _ := json.Marshal(payload)
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s/remotelink", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(issueKey))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("jira remotelink status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func buildJiraADFDescription(is ghIssue) *jiraADFDoc {
	// Create a simple document with: link to GH, state, labels, and a truncated body
	nodes := []jiraADFNode{}

	// Title paragraph with link
	linkText := fmt.Sprintf("GitHub Issue #%d", is.Number)
	nodes = append(nodes, paragraphWithLink(linkText, is.HTMLURL))

	// Meta info
	meta := fmt.Sprintf("State: %s\nLabels: %s", is.State, joinLabelNames(is.Labels))
	for _, ln := range strings.Split(meta, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		nodes = append(nodes, paragraphText(ln))
	}

	// Separator
	nodes = append(nodes, paragraphText("---"))

	// Body content (best-effort: split lines) with truncation to avoid too many nodes
	const maxBodyParagraphs = 120
	const maxBodyChars = 8000
	const maxLineChars = 500
	cleaned := sanitizeUTF8(is.Body)
	lines := strings.Split(cleaned, "\n")
	paraCount := 0
	charCount := 0
	truncated := false
	for _, ln := range lines {
		if paraCount >= maxBodyParagraphs || charCount >= maxBodyChars {
			truncated = true
			break
		}
		// Skip empty lines to reduce node count
		if strings.TrimSpace(ln) == "" {
			continue
		}
		// Strip markdown heading markers
		ln = stripMarkdownHeader(ln)
		// Cap individual line length
		if len(ln) > maxLineChars {
			ln = ln[:maxLineChars]
		}
		// Enforce total char limit
		if charCount+len(ln) > maxBodyChars {
			ln = ln[:maxBodyChars-charCount]
			truncated = true
		}
		nodes = append(nodes, paragraphText(ln))
		paraCount++
		charCount += len(ln)
	}
	if truncated {
		nodes = append(nodes, paragraphText("(truncated)"))
	}

	return &jiraADFDoc{
		Type:    "doc",
		Version: 1,
		Content: nodes,
	}
}

func paragraphText(t string) jiraADFNode {
	return jiraADFNode{
		Type: "paragraph",
		Content: []jiraADFNode{
			{Type: "text", Text: t},
		},
	}
}

func paragraphWithLink(text, href string) jiraADFNode {
	return jiraADFNode{
		Type: "paragraph",
		Content: []jiraADFNode{
			{Type: "text", Text: text, Marks: []jiraADFMark{{Type: "link", Attrs: map[string]any{"href": href}}}},
		},
	}
}

func joinLabelNames(labels []ghLabel) string {
	if len(labels) == 0 {
		return ""
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return strings.Join(out, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func uniqueStrings(in []string) []string {
	m := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := m[v]; !ok {
			m[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

var mdHeaderRe = regexp.MustCompile(`^\s*#{1,6}\s*`)

func stripMarkdownHeader(s string) string {
	return mdHeaderRe.ReplaceAllString(s, "")
}

func sanitizeUTF8(s string) string {
	// Replace NULs and ensure valid UTF-8 for JSON
	s = strings.ReplaceAll(s, "\x00", " ")
	if !json.Valid([]byte("\"" + s + "\"")) {
		// Replace problematic runes (quick fallback)
		b := make([]rune, 0, len(s))
		for _, r := range s {
			if r == '\uFFFD' || r == 0 {
				continue
			}
			b = append(b, r)
		}
		return string(b)
	}
	return s
}
