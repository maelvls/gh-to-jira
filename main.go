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
	"os/signal"
	"strings"
	"syscall"
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

// Milestones removed; selection is driven by GitHub labels

// Jira models (trimmed)
type jiraIssueCreateRequest struct {
	Fields map[string]any `json:"fields"`
}

type jiraProjectRef struct {
	ID  string `json:"id,omitempty"`
	Key string `json:"key,omitempty"`
}

type jiraIssueTypeRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// CreateMeta types (trimmed)
type jiraCreateMetaResponse struct {
	Projects []struct {
		ID         string `json:"id"`
		Key        string `json:"key"`
		Issuetypes []struct {
			ID     string                       `json:"id"`
			Name   string                       `json:"name"`
			Fields map[string]jiraFieldMetaInfo `json:"fields"`
		} `json:"issuetypes"`
	} `json:"projects"`
}

type jiraFieldMetaInfo struct {
	Required      bool             `json:"required"`
	Name          string           `json:"name"`
	Schema        jiraFieldSchema  `json:"schema"`
	AllowedValues []map[string]any `json:"allowedValues"`
	DefaultValue  any              `json:"defaultValue"`
}

type jiraFieldSchema struct {
	Type   string `json:"type"`
	Items  string `json:"items,omitempty"`
	Custom string `json:"custom,omitempty"`
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	runCycle(ctx, cfg)

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutdown signal received, exiting")
			return
		case <-ticker.C:
			runCycle(ctx, cfg)
		}
	}
}

func runCycle(ctx context.Context, cfg config) {
	start := time.Now()
	issues, err := fetchGitHubIssues(ctx, cfg)
	if err != nil {
		log.Printf("github fetch error: %v", err)
		return
	}

	log.Printf("fetched %d issues/PRs in %s/%s with label %q (state=all)", len(issues), cfg.GitHubOwner, cfg.GitHubRepo, cfg.GitHubLabel)

	for _, is := range issues {
		existsKey, err := jiraFindExisting(ctx, cfg, is.Number)
		if err != nil {
			log.Printf("warn: jira search failed for #%d: %v", is.Number, err)
			continue
		}
		if existsKey != "" {
			if cfg.DryRun {
				log.Printf("[dry-run] would update Jira %s for #%d %q", existsKey, is.Number, is.Title)
				continue
			}
			if err := jiraUpdateFromGitHubIssue(ctx, cfg, existsKey, is); err != nil {
				log.Printf("error: updating Jira %s for #%d failed: %v", existsKey, is.Number, err)
			} else {
				log.Printf("updated Jira issue %s for GitHub #%d", existsKey, is.Number)
			}
			continue
		}

		if cfg.DryRun {
			log.Printf("[dry-run] would create Jira for #%d %q", is.Number, is.Title)
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

	log.Printf("cycle completed in %s", time.Since(start).Truncate(time.Millisecond))
}

// Config
type config struct {
	GitHubToken string
	GitHubOwner string
	GitHubRepo  string
	GitHubLabel string

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
		GitHubLabel:         getenvDefault("GITHUB_LABEL", "cybr"),
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
	for {
		reqURL := fmt.Sprintf("%s?state=all&labels=%s&per_page=%d&page=%d", base, url.QueryEscape(cfg.GitHubLabel), perPage, page)
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

// milestone logic removed

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
	fields, err := buildCreateFieldsMap(ctx, cfg, is)
	if err != nil {
		return "", err
	}
	payload := jiraIssueCreateRequest{Fields: fields}

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
		if resp.StatusCode == 400 && !cfg.JiraSkipDescription && payload.Fields["description"] != nil {
			// Retry once without description in case ADF or required fields cause INVALID_INPUT
			payload.Fields["description"] = nil
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

// jiraUpdateFromGitHubIssue updates summary, labels, and description of an existing Jira issue
func jiraUpdateFromGitHubIssue(ctx context.Context, cfg config, issueKey string, is ghIssue) error {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	summary := fmt.Sprintf("cert-manager#%d: %s", is.Number, truncate(is.Title, 200))

	// Merge labels with existing to avoid losing data
	existing, _ := jiraGetIssueLabels(ctx, cfg, issueKey)
	desired := uniqueStrings(append(existing, []string{"github", "cert-manager", cfg.GitHubLabel}...))

	var desc *jiraADFDoc
	if !cfg.JiraSkipDescription {
		desc = buildJiraADFDescription(is)
	}
	fields := map[string]any{
		"summary": summary,
		"labels":  desired,
	}
	if desc != nil {
		fields["description"] = desc
	}
	payload := jiraIssueCreateRequest{Fields: fields}

	b, _ := json.Marshal(payload)
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(issueKey))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return nil
	}
	// Retry without description if 400
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode == 400 && !cfg.JiraSkipDescription && desc != nil {
		payload.Fields["description"] = nil
		b2, _ := json.Marshal(payload)
		req2, _ := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, strings.NewReader(string(b2)))
		req2.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
		req2.Header.Set("Accept", "application/json")
		req2.Header.Set("Content-Type", "application/json")
		resp2, err2 := client.Do(req2)
		if err2 != nil {
			return fmt.Errorf("jira update retry failed: %v (first: %s)", err2, string(body))
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == 204 {
			return nil
		}
		body2, _ := io.ReadAll(io.LimitReader(resp2.Body, 8192))
		return fmt.Errorf("jira update status %d: %s | retry without description status %d: %s", resp.StatusCode, string(body), resp2.StatusCode, string(body2))
	}
	return fmt.Errorf("jira update status %d: %s", resp.StatusCode, string(body))
}

func jiraGetIssueLabels(ctx context.Context, cfg config, issueKey string) ([]string, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=labels", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(issueKey))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jira get issue labels status %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Fields struct {
			Labels []string `json:"labels"`
		} `json:"fields"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Fields.Labels, nil
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

// buildCreateFieldsMap composes the fields map for creating a Jira issue,
// augmenting with required fields from CreateMeta when possible.
func buildCreateFieldsMap(ctx context.Context, cfg config, is ghIssue) (map[string]any, error) {
	summary := fmt.Sprintf("cert-manager#%d: %s", is.Number, truncate(is.Title, 200))
	labels := uniqueStrings([]string{"github", "cert-manager", cfg.GitHubLabel})
	fields := map[string]any{
		"summary": summary,
		"labels":  labels,
	}
	if !cfg.JiraSkipDescription {
		if desc := buildJiraADFDescription(is); desc != nil {
			fields["description"] = desc
		}
	}

	// Project/issuetype references
	if cfg.JiraProjectID != "" {
		fields["project"] = map[string]any{"id": cfg.JiraProjectID}
	} else {
		fields["project"] = map[string]any{"key": cfg.JiraProjectKey}
	}
	if cfg.JiraIssueTypeID != "" {
		fields["issuetype"] = map[string]any{"id": cfg.JiraIssueTypeID}
	} else {
		fields["issuetype"] = map[string]any{"name": cfg.JiraIssueType}
	}

	// Enhance with required fields from CreateMeta
	reqFields, err := fetchCreateMetaRequiredFields(ctx, cfg)
	if err != nil {
		return fields, nil // proceed without meta if it fails
	}
	enhanceFieldsWithMeta(fields, reqFields)
	return fields, nil
}

// fetchCreateMetaRequiredFields fetches required fields for the configured project and issuetype.
func fetchCreateMetaRequiredFields(ctx context.Context, cfg config) (map[string]jiraFieldMetaInfo, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	base := fmt.Sprintf("%s/rest/api/3/issue/createmeta", strings.TrimRight(cfg.JiraBaseURL, "/"))
	q := url.Values{}
	if cfg.JiraProjectID != "" {
		q.Set("projectIds", cfg.JiraProjectID)
	} else if cfg.JiraProjectKey != "" {
		q.Set("projectKeys", cfg.JiraProjectKey)
	}
	if cfg.JiraIssueTypeID != "" {
		q.Set("issuetypeIds", cfg.JiraIssueTypeID)
	} else if cfg.JiraIssueType != "" {
		q.Set("issuetypeNames", cfg.JiraIssueType)
	}
	q.Set("expand", "projects.issuetypes.fields")
	reqURL := base + "?" + q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("createmeta status %d: %s", resp.StatusCode, string(b))
	}
	var meta jiraCreateMetaResponse
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, err
	}
	for _, p := range meta.Projects {
		for _, it := range p.Issuetypes {
			if (cfg.JiraIssueTypeID != "" && it.ID == cfg.JiraIssueTypeID) || strings.EqualFold(it.Name, cfg.JiraIssueType) || cfg.JiraIssueType == "" {
				return it.Fields, nil
			}
		}
	}
	return nil, fmt.Errorf("issuetype %q not found in createmeta", cfg.JiraIssueType)
}

// enhanceFieldsWithMeta adds minimal values for required fields not already present.
func enhanceFieldsWithMeta(fields map[string]any, meta map[string]jiraFieldMetaInfo) {
	skip := map[string]struct{}{
		"summary": {}, "project": {}, "issuetype": {}, "labels": {}, "description": {},
	}
	for key, info := range meta {
		if !info.Required {
			continue
		}
		if _, ok := skip[key]; ok {
			continue
		}
		if _, exists := fields[key]; exists {
			continue
		}

		// Prefer default value
		if info.DefaultValue != nil {
			fields[key] = info.DefaultValue
			continue
		}
		// Prefer first allowed value
		if len(info.AllowedValues) > 0 {
			// Handle arrays vs single
			if info.Schema.Type == "array" {
				v := normalizeAllowedValue(info.AllowedValues[0])
				fields[key] = []any{v}
			} else {
				fields[key] = normalizeAllowedValue(info.AllowedValues[0])
			}
			continue
		}
		// Fallback by schema type
		switch info.Schema.Type {
		case "string":
			fields[key] = "Auto"
		case "number":
			fields[key] = 0
		case "date":
			fields[key] = time.Now().Format("2006-01-02")
		case "datetime":
			fields[key] = time.Now().UTC().Format("2006-01-02T15:04:05.000+0000")
		case "array":
			fields[key] = []any{}
		default:
			// leave unset if we don't know
		}
	}
}

func normalizeAllowedValue(v map[string]any) any {
	// Common shapes: {"id":"10000"}, {"value":"X"}, {"name":"X"}
	if id, ok := v["id"]; ok {
		return map[string]any{"id": id}
	}
	if val, ok := v["value"]; ok {
		return map[string]any{"value": val}
	}
	if name, ok := v["name"]; ok {
		return map[string]any{"name": name}
	}
	return v
}

func buildJiraADFDescription(is ghIssue) *jiraADFDoc {
	// Create a simple document with a link and truncated body
	nodes := []jiraADFNode{}
	linkText := fmt.Sprintf("GitHub Item #%d", is.Number)
	nodes = append(nodes, paragraphWithLink(linkText, is.HTMLURL))

	// Truncated body: limit paragraphs and total chars
	const maxBodyParagraphs = 120
	const maxBodyChars = 8000
	const maxLineChars = 500
	cleaned := is.Body
	lines := strings.Split(cleaned, "\n")
	paraCount := 0
	charCount := 0
	for _, ln := range lines {
		if paraCount >= maxBodyParagraphs || charCount >= maxBodyChars {
			nodes = append(nodes, paragraphText("(truncated)"))
			break
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if len(ln) > maxLineChars {
			ln = ln[:maxLineChars]
		}
		if charCount+len(ln) > maxBodyChars {
			ln = ln[:maxBodyChars-charCount]
		}
		nodes = append(nodes, paragraphText(ln))
		paraCount++
		charCount += len(ln)
	}

	return &jiraADFDoc{Type: "doc", Version: 1, Content: nodes}
}

func paragraphWithLink(text, href string) jiraADFNode {
	return jiraADFNode{
		Type: "paragraph",
		Content: []jiraADFNode{
			{Type: "text", Text: text, Marks: []jiraADFMark{{Type: "link", Attrs: map[string]any{"href": href}}}},
		},
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
