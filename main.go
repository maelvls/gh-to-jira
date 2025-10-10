package main

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"flag"
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

	"github.com/go-yaml/yaml"
)

const (
	jiraCapacityCategoryField = "customfield_10412"
	githubJiraBacklinkMarker  = "<!-- do not edit this line, will be re-added automatically -->"
)

// GitHub models (trimmed)
type ghLabel struct {
	Name string `json:"name"`
}

type ghIssueType struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type ghUser struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type ghReviewer struct {
	User ghUser `json:"user,omitzero"`
}

type ghReviewRequest struct {
	Users []ghUser `json:"users,omitzero"`
}

// UserConfig represents the YAML configuration for user mappings and application settings
type UserConfig struct {
	// GitHub configuration
	GitHubOwner string   `yaml:"github_owner"`
	GitHubRepo  string   `yaml:"github_repo"`
	GitHubRepos []string `yaml:"github_repos"`
	GitHubLabel string   `yaml:"github_label"`

	// Jira project configuration
	JiraProjectKey      string `yaml:"jira_project_key"`
	JiraProjectID       string `yaml:"jira_project_id"`
	JiraIssueType       string `yaml:"jira_issue_type"`
	JiraIssueTypeID     string `yaml:"jira_issue_type_id"`
	JiraSkipDescription bool   `yaml:"jira_skip_description"`

	// Jira status mapping
	JiraStatusOpen     string `yaml:"jira_status_open"`
	JiraStatusClosed   string `yaml:"jira_status_closed"`
	JiraStatusDraft    string `yaml:"jira_status_draft"`
	JiraStatusReopened string `yaml:"jira_status_reopened"`

	// Jira resolution (for closing tickets)
	JiraResolution string `yaml:"jira_resolution"`

	// Jira team field configuration
	JiraTeamFieldKey string `yaml:"jira_team_field_key"` // e.g., customfield_10211
	JiraTeamOptionID string `yaml:"jira_team_option_id"` // e.g., 13667

	// Jira components configuration
	JiraDefaultComponent string            `yaml:"jira_default_component"`
	JiraComponents       map[string]string `yaml:"jira_components"`

	// User mappings
	GitHubToJiraUsers  map[string]string `yaml:"github_to_jira_users"`
	CyberArkKnownUsers []string          `yaml:"cyberark_known_users"`
}

type ghIssue struct {
	Number      int         `json:"number"`
	Title       string      `json:"title"`
	Body        string      `json:"body"`
	HTMLURL     string      `json:"html_url"`
	Labels      []ghLabel   `json:"labels"`
	PullRequest *struct{}   `json:"pull_request,omitzero"`
	State       string      `json:"state"`
	Draft       bool        `json:"draft,omitzero"`
	Merged      bool        `json:"merged,omitzero"`
	IssueType   ghIssueType `json:"issue_type,omitzero"`
	User        ghUser      `json:"user,omitzero"`      // Author
	Assignee    ghUser      `json:"assignee,omitzero"`  // Single assignee
	Assignees   []ghUser    `json:"assignees,omitzero"` // Multiple assignees
}

// Milestones removed; selection is driven by GitHub labels

// Jira models (trimmed)
type jiraIssueCreateRequest struct {
	Fields map[string]any `json:"fields"`
}

type jiraProjectRef struct {
	ID  string `json:"id,omitzero"`
	Key string `json:"key,omitzero"`
}

type jiraIssueTypeRef struct {
	ID   string `json:"id,omitzero"`
	Name string `json:"name,omitzero"`
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
	Items  string `json:"items,omitzero"`
	Custom string `json:"custom,omitzero"`
}

// ADF (very minimal)
type jiraADFDoc struct {
	Type    string        `json:"type"`
	Version int           `json:"version"`
	Content []jiraADFNode `json:"content"`
}

type jiraADFNode struct {
	Type    string         `json:"type"`
	Content []jiraADFNode  `json:"content,omitzero"`
	Text    string         `json:"text,omitzero"`
	Marks   []jiraADFMark  `json:"marks,omitzero"`
	Attrs   map[string]any `json:"attrs,omitzero"`
}

type jiraADFMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitzero"`
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
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Status struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"status,omitzero"`
	} `json:"fields,omitzero"`
}

type jiraJQLBatchRequest struct {
	Queries []jiraJQLQuery `json:"queries"`
}

type jiraJQLQuery struct {
	JQL        string   `json:"jql"`
	StartAt    int      `json:"startAt,omitzero"`
	MaxResults int      `json:"maxResults,omitzero"`
	Fields     []string `json:"fields,omitzero"`
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

// Jira transition models
type jiraTransition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"to"`
}

type jiraTransitionsResponse struct {
	Transitions []jiraTransition `json:"transitions"`
}

type jiraTransitionRequest struct {
	Transition struct {
		ID string `json:"id"`
	} `json:"transition"`
	Fields map[string]any `json:"fields,omitzero"`
}

func main() {
	// Parse command-line flags
	var dryRun = flag.Bool("dry-run", false, "only print actions without making changes")
	var configPathFlag = flag.String("config", "", "path to YAML configuration file (overrides CONFIG_PATH env var)")
	flag.Parse()

	cfg, err := loadConfig(*dryRun, *configPathFlag)
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
	repos, err := resolveRepos(ctx, cfg)
	if err != nil {
		log.Printf("error resolving repositories: %v", err)
		return
	}
	if len(repos) == 0 {
		log.Printf("no repositories resolved for %s", cfg.UserConfig.GitHubOwner)
		return
	}

	for _, repo := range repos {
		select {
		case <-ctx.Done():
			log.Printf("cycle interrupted while processing %s/%s", cfg.UserConfig.GitHubOwner, repo)
			return
		default:
		}

		issues, err := fetchGitHubIssues(ctx, cfg, repo)
		if err != nil {
			log.Printf("github fetch error for %s/%s: %v", cfg.UserConfig.GitHubOwner, repo, err)
			continue
		}

		log.Printf("fetched %d issues/PRs in %s/%s with label %q", len(issues), cfg.UserConfig.GitHubOwner, repo, cfg.UserConfig.GitHubLabel)

		for _, is := range issues {
			// Use the new function that includes status information
			existingIssue, err := jiraFindExistingWithStatus(ctx, cfg, repo, is.Number)
			if err != nil {
				log.Printf("warn: jira search failed for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
				continue
			}
			if existingIssue != nil {
				if cfg.DryRun {
					desiredStatus := getDesiredJiraStatus(cfg, is)
					currentStatus := existingIssue.Fields.Status.Name
					statusMsg := ""
					if desiredStatus != "" {
						if desiredStatus == "NOT_CLOSED" {
							// Handle the "NOT_CLOSED" rule
							if strings.EqualFold(currentStatus, cfg.UserConfig.JiraStatusClosed) {
								statusMsg = fmt.Sprintf(" (status transition: %q -> %q - reopening)", currentStatus, cfg.UserConfig.JiraStatusReopened)
							} else {
								statusMsg = fmt.Sprintf(" (status: %q - acceptable for open GitHub item)", currentStatus)
							}
						} else if currentStatus != desiredStatus {
							statusMsg = fmt.Sprintf(" (status transition: %q -> %q)", currentStatus, desiredStatus)
						} else {
							statusMsg = fmt.Sprintf(" (status: already %q)", currentStatus)
						}
					}
					assigneeInfo := ""
					if assigneeAccountID, err := determineJiraAssignee(ctx, cfg, repo, is); err != nil {
						assigneeInfo = " (assignee determination failed)"
					} else if assigneeAccountID != "" {
						assigneeInfo = fmt.Sprintf(" (would assign to: %s)", assigneeAccountID)
					} else {
						assigneeInfo = " (no assignee mapping found)"
					}
					log.Printf("[dry-run] would update Jira %s for %s/%s#%d %q%s%s", existingIssue.Key, cfg.UserConfig.GitHubOwner, repo, is.Number, is.Title, statusMsg, assigneeInfo)
					continue
				}
				if err := jiraUpdateFromGitHubIssueWithStatus(ctx, cfg, existingIssue, repo, is); err != nil {
					log.Printf("error: updating Jira %s for %s/%s#%d failed: %v", existingIssue.Key, cfg.UserConfig.GitHubOwner, repo, is.Number, err)
				} else {
					if err := ensureGitHubJiraBacklink(ctx, cfg, repo, is, existingIssue.Key); err != nil {
						log.Printf("warn: %v", err)
					}
					log.Printf("updated Jira issue %s for %s/%s#%d", existingIssue.Key, cfg.UserConfig.GitHubOwner, repo, is.Number)
				}
				continue
			}

			if cfg.DryRun {
				assigneeInfo := ""
				if assigneeAccountID, err := determineJiraAssignee(ctx, cfg, repo, is); err != nil {
					assigneeInfo = " (assignee determination failed)"
				} else if assigneeAccountID != "" {
					assigneeInfo = fmt.Sprintf(" (would assign to: %s)", assigneeAccountID)
				} else {
					assigneeInfo = " (no assignee mapping found)"
				}
				log.Printf("[dry-run] would create Jira for %s/%s#%d %q%s", cfg.UserConfig.GitHubOwner, repo, is.Number, is.Title, assigneeInfo)
				continue
			}
			key, err := jiraCreateFromGitHubIssue(ctx, cfg, repo, is)
			if err != nil {
				log.Printf("error: creating Jira for %s/%s#%d failed: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
				continue
			}
			log.Printf("created Jira issue %s for %s/%s#%d", key, cfg.UserConfig.GitHubOwner, repo, is.Number)

			if err := jiraAddRemoteLink(ctx, cfg, key, is.HTMLURL, is.Title); err != nil {
				log.Printf("warn: adding remote link to %s failed for %s/%s#%d: %v", key, cfg.UserConfig.GitHubOwner, repo, is.Number, err)
			}
			if err := ensureGitHubJiraBacklink(ctx, cfg, repo, is, key); err != nil {
				log.Printf("warn: %v", err)
			}
		}
	}

	log.Printf("cycle completed in %s", time.Since(start).Truncate(time.Millisecond))
}

func resolveRepos(ctx context.Context, cfg config) ([]string, error) {
	if len(cfg.UserConfig.GitHubRepos) > 0 {
		return cfg.UserConfig.GitHubRepos, nil
	}
	if cfg.UserConfig.GitHubRepo != "" {
		return []string{cfg.UserConfig.GitHubRepo}, nil
	}
	return listOrgRepos(ctx, cfg)
}

// Config
type config struct {
	// Secret environment variables (not in YAML)
	GitHubToken  string
	JiraBaseURL  string
	JiraEmail    string
	JiraAPIToken string

	// Configuration loaded from YAML file
	ConfigPath string     // Path to the YAML configuration file
	UserConfig UserConfig // Loaded user configuration

	// Runtime flags
	DryRun bool

	HTTPTimeout time.Duration
}

func loadConfig(dryRun bool, configPathFlag string) (config, error) {
	cfg := config{
		// Only load secrets from environment variables
		GitHubToken:  os.Getenv("GITHUB_TOKEN"),
		JiraBaseURL:  os.Getenv("JIRA_BASE_URL"),
		JiraEmail:    os.Getenv("JIRA_EMAIL"),
		JiraAPIToken: os.Getenv("JIRA_API_TOKEN"),
		ConfigPath:   resolveConfigPath(configPathFlag),
		DryRun:       dryRun,
		HTTPTimeout:  20 * time.Second,
	}

	// Load user configuration from YAML file (includes all non-secret config)
	if err := loadUserConfig(&cfg); err != nil {
		return cfg, fmt.Errorf("failed to load config: %v", err)
	}

	// Validate required secret environment variables
	if cfg.GitHubToken == "" {
		return cfg, errors.New("missing GITHUB_TOKEN")
	}
	if cfg.JiraBaseURL == "" || cfg.JiraEmail == "" || cfg.JiraAPIToken == "" {
		return cfg, errors.New("missing one or more Jira envs: JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN")
	}

	// Validate required YAML configuration
	if cfg.UserConfig.JiraProjectKey == "" {
		return cfg, errors.New("missing jira_project_key in config file")
	}

	return cfg, nil
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func resolveConfigPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return getenvDefault("CONFIG_PATH", "config.yaml")
}

// loadUserConfig loads user configuration from a YAML file
func loadUserConfig(cfg *config) error {
	// Set defaults for all configuration values
	cfg.UserConfig = UserConfig{
		GitHubOwner:         "cert-manager",
		GitHubLabel:         "cybr",
		JiraIssueType:       "Task",
		JiraStatusOpen:      "To Do",
		JiraStatusClosed:    "Done",
		JiraStatusDraft:     "In Progress",
		JiraStatusReopened:  "Reopened",
		JiraResolution:      "Done",
		GitHubToJiraUsers:   make(map[string]string),
		CyberArkKnownUsers:  []string{},
		JiraSkipDescription: true,
	}

	// Check if file exists
	if _, err := os.Stat(cfg.ConfigPath); os.IsNotExist(err) {
		log.Printf("config file %s not found, using defaults", cfg.ConfigPath)
		return nil
	}

	// Read the YAML file
	data, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read config file %s: %v", cfg.ConfigPath, err)
	}

	// Parse YAML and merge with defaults
	var userConfig UserConfig
	if err := yaml.Unmarshal(data, &userConfig); err != nil {
		return fmt.Errorf("failed to parse config YAML: %v", err)
	}

	// Merge loaded config with defaults (only override non-zero values)
	if userConfig.GitHubOwner != "" {
		cfg.UserConfig.GitHubOwner = userConfig.GitHubOwner
	}
	if userConfig.GitHubRepo != "" {
		cfg.UserConfig.GitHubRepo = userConfig.GitHubRepo
	}
	if len(userConfig.GitHubRepos) > 0 {
		cfg.UserConfig.GitHubRepos = userConfig.GitHubRepos
	}
	if userConfig.GitHubLabel != "" {
		cfg.UserConfig.GitHubLabel = userConfig.GitHubLabel
	}
	if userConfig.JiraProjectKey != "" {
		cfg.UserConfig.JiraProjectKey = userConfig.JiraProjectKey
	}
	if userConfig.JiraProjectID != "" {
		cfg.UserConfig.JiraProjectID = userConfig.JiraProjectID
	}
	if userConfig.JiraIssueType != "" {
		cfg.UserConfig.JiraIssueType = userConfig.JiraIssueType
	}
	if userConfig.JiraIssueTypeID != "" {
		cfg.UserConfig.JiraIssueTypeID = userConfig.JiraIssueTypeID
	}
	if userConfig.JiraStatusOpen != "" {
		cfg.UserConfig.JiraStatusOpen = userConfig.JiraStatusOpen
	}
	if userConfig.JiraStatusClosed != "" {
		cfg.UserConfig.JiraStatusClosed = userConfig.JiraStatusClosed
	}
	if userConfig.JiraStatusDraft != "" {
		cfg.UserConfig.JiraStatusDraft = userConfig.JiraStatusDraft
	}
	if userConfig.JiraStatusReopened != "" {
		cfg.UserConfig.JiraStatusReopened = userConfig.JiraStatusReopened
	}
	if userConfig.JiraResolution != "" {
		cfg.UserConfig.JiraResolution = userConfig.JiraResolution
	}
	if len(userConfig.GitHubToJiraUsers) > 0 {
		cfg.UserConfig.GitHubToJiraUsers = userConfig.GitHubToJiraUsers
	}
	if len(userConfig.CyberArkKnownUsers) > 0 {
		cfg.UserConfig.CyberArkKnownUsers = userConfig.CyberArkKnownUsers
	}
	// For booleans, we need to check if they were explicitly set in YAML
	// This is a limitation of Go YAML parsing - we'll accept any explicit value
	cfg.UserConfig.JiraSkipDescription = userConfig.JiraSkipDescription

	log.Printf("loaded config: %d user mappings, %d CyberArk users",
		len(cfg.UserConfig.GitHubToJiraUsers), len(cfg.UserConfig.CyberArkKnownUsers))

	return nil
}

func listOrgRepos(ctx context.Context, cfg config) ([]string, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	perPage := 100
	page := 1
	var repos []string
	base := fmt.Sprintf("https://api.github.com/orgs/%s/repos", url.PathEscape(cfg.UserConfig.GitHubOwner))
	for {
		reqURL := fmt.Sprintf("%s?per_page=%d&page=%d", base, perPage, page)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "gh-to-jira-bot")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("github repos: status %d: %s", resp.StatusCode, string(body))
		}
		var pageItems []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &pageItems); err != nil {
			return nil, err
		}
		if len(pageItems) == 0 {
			break
		}
		for _, repo := range pageItems {
			repos = append(repos, repo.Name)
		}
		if len(pageItems) < perPage {
			break
		}
		page++
	}
	return repos, nil
}

// GitHub client
func fetchGitHubIssues(ctx context.Context, cfg config, repo string) ([]ghIssue, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	var all []ghIssue
	perPage := 100
	page := 1
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", url.PathEscape(cfg.UserConfig.GitHubOwner), url.PathEscape(repo))
	for {
		reqURL := fmt.Sprintf("%s?state=all&labels=%s&per_page=%d&page=%d", base, url.QueryEscape(cfg.UserConfig.GitHubLabel), perPage, page)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "gh-to-jira-bot")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("github: status %d: %s", resp.StatusCode, string(body))
		}
		var pageItems []ghIssue
		if err := json.Unmarshal(body, &pageItems); err != nil {
			return nil, err
		}

		// For PRs, fetch additional details including merged status
		for i := range pageItems {
			if pageItems[i].PullRequest != nil {
				prDetails, err := fetchGitHubPRDetails(ctx, cfg, repo, pageItems[i].Number)
				if err != nil {
					log.Printf("warn: failed to fetch PR details for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, pageItems[i].Number, err)
				} else {
					pageItems[i].Merged = prDetails.Merged
					pageItems[i].Draft = prDetails.Draft
				}
			}
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

// fetchGitHubPRDetails fetches additional PR details including merged status
func fetchGitHubPRDetails(ctx context.Context, cfg config, repo string, prNumber int) (*ghIssue, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", url.PathEscape(cfg.UserConfig.GitHubOwner), url.PathEscape(repo), prNumber)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return nil, fmt.Errorf("github PR details: status %d: %s", resp.StatusCode, string(body))
	}
	var pr ghIssue
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// fetchGitHubPRReviewers fetches the reviewers for a PR
func fetchGitHubPRReviewers(ctx context.Context, cfg config, repo string, prNumber int) ([]ghUser, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d/requested_reviewers", url.PathEscape(cfg.UserConfig.GitHubOwner), url.PathEscape(repo), prNumber)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return nil, fmt.Errorf("github PR reviewers: status %d: %s", resp.StatusCode, string(body))
	}
	var reviewRequest ghReviewRequest
	if err := json.NewDecoder(resp.Body).Decode(&reviewRequest); err != nil {
		return nil, err
	}
	return reviewRequest.Users, nil
}

func ensureGitHubJiraBacklink(ctx context.Context, cfg config, repo string, is ghIssue, jiraKey string) error {
	current, err := githubGetIssue(ctx, cfg, repo, is.Number)
	if err != nil {
		return fmt.Errorf("fetching GitHub issue %s/%s#%d failed: %w", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
	}
	targetLine := buildJiraBacklinkLine(cfg, jiraKey)
	if strings.Contains(current.Body, targetLine) {
		log.Printf("backlink already present on %s/%s#%d, skipping update", cfg.UserConfig.GitHubOwner, repo, is.Number)
		return nil
	}
	updatedBody, changed := ensureBacklinkLine(current.Body, targetLine)
	if !changed {
		log.Printf("backlink unchanged after normalisation on %s/%s#%d, skipping update", cfg.UserConfig.GitHubOwner, repo, is.Number)
		return nil
	}
	log.Printf("updating GitHub description for %s/%s#%d with Jira backlink", cfg.UserConfig.GitHubOwner, repo, is.Number)
	if err := githubUpdateIssueBody(ctx, cfg, repo, is.Number, updatedBody); err != nil {
		return fmt.Errorf("updating body for %s/%s#%d failed: %w", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
	}
	log.Printf("added Jira backlink to description of %s/%s#%d", cfg.UserConfig.GitHubOwner, repo, is.Number)
	return nil
}

func githubGetIssue(ctx context.Context, cfg config, repo string, issueNumber int) (*ghIssue, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d",
		url.PathEscape(cfg.UserConfig.GitHubOwner), url.PathEscape(repo), issueNumber)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return nil, fmt.Errorf("github issue get status %d: %s", resp.StatusCode, string(body))
	}
	var issue ghIssue
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

func githubUpdateIssueBody(ctx context.Context, cfg config, repo string, issueNumber int, body string) error {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	payload := map[string]string{"body": body}
	b, _ := json.Marshal(payload)
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d",
		url.PathEscape(cfg.UserConfig.GitHubOwner), url.PathEscape(repo), issueNumber)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "gh-to-jira-bot")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github update body status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func ensureBacklinkLine(body, line string) (string, bool) {
	if strings.Contains(body, line) {
		return body, false
	}
	clean := removeBacklinkLine(body)
	trimmed := strings.TrimRight(clean, "\n")
	var updated string
	if strings.TrimSpace(trimmed) == "" {
		updated = line
	} else {
		updated = trimmed + "\n\n" + line
	}
	return updated, updated != body
}

func removeBacklinkLine(body string) string {
	if !strings.Contains(body, githubJiraBacklinkMarker) {
		return body
	}
	lines := strings.Split(body, "\n")
	var keep []string
	for _, line := range lines {
		if strings.Contains(line, githubJiraBacklinkMarker) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

func buildJiraBacklinkLine(cfg config, jiraKey string) string {
	targetURL := buildJiraBrowseURL(cfg, jiraKey)
	return fmt.Sprintf("CyberArk tracker: [%s](%s) %s", jiraKey, targetURL, githubJiraBacklinkMarker)
}

func buildJiraBrowseURL(cfg config, jiraKey string) string {
	return fmt.Sprintf("%s/browse/%s", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(jiraKey))
}

func isAnIssue(is ghIssue) bool {
	return is.PullRequest == nil
}

// determineJiraAssignee determines the best Jira assignee based on GitHub issue/PR data
func determineJiraAssignee(ctx context.Context, cfg config, repo string, is ghIssue) (string, error) {
	// Do not assign Jira issues when the GitHub issue itself has no assignee.
	if isAnIssue(is) {
		hasGitHubAssignee := false
		if is.Assignee.Login != "" {
			hasGitHubAssignee = true
		}
		if len(is.Assignees) > 0 {
			hasGitHubAssignee = true
		}
		if !hasGitHubAssignee {
			return "", nil
		}
	}

	var candidates []string

	// Create a set of CyberArk known users for quick lookup.
	cyberArkUsers := make(map[string]bool)
	for _, user := range cfg.UserConfig.CyberArkKnownUsers {
		cyberArkUsers[user] = true
	}

	// Start with the author if known at CyberArk.
	if is.User.Login != "" {
		if cyberArkUsers[is.User.Login] {
			candidates = append(candidates, is.User.Login)
		}
	}

	// Add assignees if known at CyberArk.
	for _, assignee := range is.Assignees {
		if assignee.Login != "" && cyberArkUsers[assignee.Login] {
			candidates = append(candidates, assignee.Login)
		}
	}

	// For PRs, also check reviewers.
	if is.PullRequest != nil {
		reviewers, err := fetchGitHubPRReviewers(ctx, cfg, repo, is.Number)
		if err != nil {
			log.Printf("warn: failed to fetch reviewers for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
		} else {
			for _, reviewer := range reviewers {
				if reviewer.Login != "" && cyberArkUsers[reviewer.Login] {
					candidates = append(candidates, reviewer.Login)
				}
			}
		}
	}

	// If author is not known at CyberArk, fallback to assignees and reviewers.
	if len(candidates) == 0 {
		// Add any assignee (even if not known at CyberArk).
		for _, assignee := range is.Assignees {
			if assignee.Login != "" {
				candidates = append(candidates, assignee.Login)
			}
		}

		// For PRs, add any reviewer
		if is.PullRequest != nil {
			reviewers, err := fetchGitHubPRReviewers(ctx, cfg, repo, is.Number)
			if err == nil {
				for _, reviewer := range reviewers {
					if reviewer.Login != "" {
						candidates = append(candidates, reviewer.Login)
					}
				}
			}
		}
	}

	// Remove duplicates and pick the first candidate
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if !seen[candidate] {
			seen[candidate] = true
			// Check if we have a mapping for this GitHub user
			if jiraAccountID, exists := cfg.UserConfig.GitHubToJiraUsers[candidate]; exists {
				return jiraAccountID, nil
			}
		}
	}

	return "", nil // No suitable assignee found
}

// milestone logic removed

// Jira helper: basic auth header
func jiraAuthHeader(email, token string) string {
	raw := email + ":" + token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func jiraFindExisting(ctx context.Context, cfg config, repo string, ghNumber int) (string, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	token := escapeJQLString(summaryPrefix(cfg.UserConfig.GitHubOwner, repo, ghNumber))
	jql := fmt.Sprintf(`project = %s AND environment ~ "%s"`, cfg.UserConfig.JiraProjectKey, token)
	reqURL := fmt.Sprintf("%s/rest/api/3/search/jql?jql=%s&maxResults=2&fields=id,key,status", strings.TrimRight(cfg.JiraBaseURL, "/"), url.QueryEscape(jql))
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
	}

	// Fallback: POST search
	payload := jiraJQLBatchRequest{Queries: []jiraJQLQuery{{
		JQL:        jql,
		StartAt:    0,
		MaxResults: 2,
		Fields:     []string{"id", "key", "status"},
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
		return "", fmt.Errorf("jira search status %d: %s", resp.StatusCode, string(bdy))
	}
	var out jiraJQLBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Results) > 0 && len(out.Results[0].Issues) > 0 {
		return out.Results[0].Issues[0].Key, nil
	}
	return "", nil
}

func jiraCreateFromGitHubIssue(ctx context.Context, cfg config, repo string, is ghIssue) (string, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	fields, err := buildCreateFieldsMap(ctx, cfg, repo, is)
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
		if resp.StatusCode == 400 && !cfg.UserConfig.JiraSkipDescription && payload.Fields["description"] != nil {
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

func jiraFindExistingWithStatus(ctx context.Context, cfg config, repo string, ghNumber int) (*jiraBasicIssue, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	token := escapeJQLString(summaryPrefix(cfg.UserConfig.GitHubOwner, repo, ghNumber))
	jql := fmt.Sprintf(`project = %s AND environment ~ "%s"`, cfg.UserConfig.JiraProjectKey, token)
	reqURL := fmt.Sprintf("%s/rest/api/3/search/jql?jql=%s&maxResults=2&fields=id,key,status", strings.TrimRight(cfg.JiraBaseURL, "/"), url.QueryEscape(jql))
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
					return &out.Issues[0], nil
				}
				return nil, nil
			}
		}
	}

	// Fallback: POST search
	payload := jiraJQLBatchRequest{Queries: []jiraJQLQuery{{
		JQL:        jql,
		StartAt:    0,
		MaxResults: 2,
		Fields:     []string{"id", "key", "status"},
	}}}
	b, _ := json.Marshal(payload)
	reqURL = fmt.Sprintf("%s/rest/api/3/search/jql", strings.TrimRight(cfg.JiraBaseURL, "/"))
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		bdy, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jira search status %d: %s", resp.StatusCode, string(bdy))
	}
	var out jiraJQLBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Results) > 0 && len(out.Results[0].Issues) > 0 {
		return &out.Results[0].Issues[0], nil
	}
	return nil, nil
}

// jiraUpdateFromGitHubIssue updates labels and assignee of an existing Jira issue while
// leaving summary/description untouched by default.
func jiraUpdateFromGitHubIssue(ctx context.Context, cfg config, issueKey string, repo string, is ghIssue) error {
	client := &http.Client{Timeout: cfg.HTTPTimeout}

	// Merge labels with existing to avoid losing data
	existing, _ := jiraGetIssueLabels(ctx, cfg, issueKey)
	desired := uniqueStrings(append(existing, []string{"OpenSource", "gh-to-jira", fmt.Sprintf("repo:%s", repo)}...))

	fields := map[string]any{
		"labels":      desired,
		"environment": buildEnvironmentADF(cfg.UserConfig.GitHubOwner, repo, is.Number, is.HTMLURL),
	}
	if components := determineJiraComponents(cfg.UserConfig, repo); len(components) > 0 {
		fields["components"] = components
	}

	// Ensure team remains set on updates as well, if configured
	if cfg.UserConfig.JiraTeamFieldKey != "" && cfg.UserConfig.JiraTeamOptionID != "" {
		fields[cfg.UserConfig.JiraTeamFieldKey] = map[string]any{"id": cfg.UserConfig.JiraTeamOptionID}
	}
	if capacityCategory := determineCapacityCategory(is); capacityCategory != "" {
		fields[jiraCapacityCategoryField] = map[string]any{"value": capacityCategory}
	}

	// Update assignee based on GitHub issue/PR data
	if assigneeAccountID, err := determineJiraAssignee(ctx, cfg, repo, is); err != nil {
		log.Printf("warn: failed to determine assignee for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
	} else if assigneeAccountID != "" {
		fields["assignee"] = map[string]any{"accountId": assigneeAccountID}
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return fmt.Errorf("jira update status %d: %s", resp.StatusCode, string(body))
}

// jiraUpdateFromGitHubIssueWithStatus updates labels and status of an existing Jira issue
func jiraUpdateFromGitHubIssueWithStatus(ctx context.Context, cfg config, jiraIssue *jiraBasicIssue, repo string, is ghIssue) error {
	// Update labels and environment first
	if err := jiraUpdateFromGitHubIssue(ctx, cfg, jiraIssue.Key, repo, is); err != nil {
		return err
	}

	// Determine desired Jira status based on GitHub state
	desiredStatus := getDesiredJiraStatus(cfg, is)
	if desiredStatus == "" {
		return nil // No status mapping configured
	}

	// Get current status
	currentStatus := jiraIssue.Fields.Status.Name

	// Handle the "NOT_CLOSED" rule for open GitHub issues/PRs
	if desiredStatus == "NOT_CLOSED" {
		// For open GitHub items, we want any status except the closed status
		if isClosedStatus(currentStatus) {
			// Current status is "Done/Closed" but GitHub is open - need to reopen
			log.Printf("GitHub %s/%s#%d is open but Jira issue %s is %q, attempting to reopen to %q",
				cfg.UserConfig.GitHubOwner, repo, is.Number, jiraIssue.Key, currentStatus, cfg.UserConfig.JiraStatusReopened)
			return jiraTransitionStatus(ctx, cfg, jiraIssue.Key, currentStatus, cfg.UserConfig.JiraStatusReopened)
		} else {
			// Current status is fine (not closed), leave it as is
			log.Printf("GitHub %s/%s#%d is open and Jira issue %s is %q (acceptable, not changing)",
				cfg.UserConfig.GitHubOwner, repo, is.Number, jiraIssue.Key, currentStatus)
			return nil
		}
	}

	// Handle specific status transitions
	if statusNamesMatch(currentStatus, desiredStatus) {
		log.Printf("Jira issue %s already in correct status %q", jiraIssue.Key, currentStatus)
		return nil // Already in correct status
	}

	// Log the status transition attempt
	githubState := is.State
	if is.PullRequest != nil {
		if is.Merged {
			githubState = "merged"
		} else if is.Draft {
			githubState = "draft"
		}
	}
	log.Printf("GitHub %s/%s#%d is %q, attempting to transition Jira issue %s from %q to %q",
		cfg.UserConfig.GitHubOwner, repo, is.Number, githubState, jiraIssue.Key, currentStatus, desiredStatus)

	// Perform status transition
	return jiraTransitionStatus(ctx, cfg, jiraIssue.Key, currentStatus, desiredStatus)
}

// getDesiredJiraStatus maps GitHub issue/PR state to desired Jira status
func getDesiredJiraStatus(cfg config, is ghIssue) string {
	if is.PullRequest != nil {
		// This is a PR
		if is.Merged {
			return cfg.UserConfig.JiraStatusClosed
		}
		if is.Draft {
			return cfg.UserConfig.JiraStatusDraft
		}
		if is.State == "closed" {
			return cfg.UserConfig.JiraStatusClosed
		}
		if is.State == "open" {
			// For open PRs, any status except closed/done is acceptable
			return "NOT_CLOSED" // Special marker meaning "any status except closed"
		}
	} else {
		// This is an issue
		if is.State == "closed" {
			return cfg.UserConfig.JiraStatusClosed
		}
		if is.State == "open" {
			// For open issues, any status except closed/done is acceptable
			return "NOT_CLOSED" // Special marker meaning "any status except closed"
		}
	}
	return ""
}

// isClosedStatus checks if a status name represents a closed/done state
func isClosedStatus(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	closedStatuses := []string{"done", "closed", "resolved", "complete", "completed", "finished"}
	for _, closed := range closedStatuses {
		if s == closed {
			return true
		}
	}
	return false
}

// statusNamesMatch checks if two status names should be considered equivalent
func statusNamesMatch(status1, status2 string) bool {
	// Direct case-insensitive match
	if strings.EqualFold(status1, status2) {
		return true
	}

	// If both are closed statuses, consider them equivalent
	if isClosedStatus(status1) && isClosedStatus(status2) {
		return true
	}

	return false
}

// jiraTransitionStatus transitions a Jira issue to the specified status
func jiraTransitionStatus(ctx context.Context, cfg config, issueKey, currentStatus, targetStatus string) error {
	// Get available transitions
	transitions, err := jiraGetTransitions(ctx, cfg, issueKey)
	if err != nil {
		return fmt.Errorf("failed to get transitions for %s: %v", issueKey, err)
	}

	// Find transition to target status
	var transitionID string
	var matchedStatusName string
	for _, t := range transitions {
		if statusNamesMatch(t.To.Name, targetStatus) {
			transitionID = t.ID
			matchedStatusName = t.To.Name
			break
		}
	}

	if transitionID == "" {
		// No direct transition available to target status
		// Build list of available transition targets
		var availableTargets []string
		for _, t := range transitions {
			availableTargets = append(availableTargets, t.To.Name)
		}

		if len(availableTargets) > 0 {
			log.Printf("warn: no transition available from %q to %q for %s, only possible transitions are: %s", currentStatus, targetStatus, issueKey, strings.Join(availableTargets, ", "))
		} else {
			log.Printf("warn: no transition available from %q to %q for %s, no transitions available", currentStatus, targetStatus, issueKey)
		}
		return nil
	}

	// Execute transition
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	payload := jiraTransitionRequest{
		Transition: struct {
			ID string `json:"id"`
		}{ID: transitionID},
	}

	// If transitioning to closed status, include resolution field
	if isClosedStatus(targetStatus) || isClosedStatus(matchedStatusName) {
		payload.Fields = map[string]any{
			"resolution": map[string]any{
				"name": cfg.UserConfig.JiraResolution,
			},
		}
	}

	b, _ := json.Marshal(payload)
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s/transitions", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(issueKey))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(b)))
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		if matchedStatusName != "" && !strings.EqualFold(matchedStatusName, targetStatus) {
			log.Printf("transitioned Jira issue %s from %q to %q (matched as %q)", issueKey, currentStatus, targetStatus, matchedStatusName)
		} else {
			log.Printf("transitioned Jira issue %s from %q to %q", issueKey, currentStatus, targetStatus)
		}
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return fmt.Errorf("jira transition status %d: %s", resp.StatusCode, string(body))
}

// jiraGetTransitions gets available transitions for a Jira issue
func jiraGetTransitions(ctx context.Context, cfg config, issueKey string) ([]jiraTransition, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s/transitions", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(issueKey))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jira get transitions status %d: %s", resp.StatusCode, string(body))
	}
	var out jiraTransitionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Transitions, nil
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
func buildCreateFieldsMap(ctx context.Context, cfg config, repo string, is ghIssue) (map[string]any, error) {
	summary := buildSummary(repo, is.Number, is.Title)
	labels := uniqueStrings([]string{"OpenSource", "gh-to-jira", fmt.Sprintf("repo:%s", repo)})
	fields := map[string]any{
		"summary": summary,
		"labels":  labels,
	}
	if components := determineJiraComponents(cfg.UserConfig, repo); len(components) > 0 {
		fields["components"] = components
	}

	// Set Team via configured custom field/option id when provided
	if cfg.UserConfig.JiraTeamFieldKey != "" && cfg.UserConfig.JiraTeamOptionID != "" {
		fields[cfg.UserConfig.JiraTeamFieldKey] = map[string]any{"id": cfg.UserConfig.JiraTeamOptionID}
	}
	if capacityCategory := determineCapacityCategory(is); capacityCategory != "" {
		fields[jiraCapacityCategoryField] = map[string]any{"value": capacityCategory}
	}
	fields["environment"] = buildEnvironmentADF(cfg.UserConfig.GitHubOwner, repo, is.Number, is.HTMLURL)
	if !cfg.UserConfig.JiraSkipDescription {
		if desc := buildJiraADFDescription(cfg.UserConfig.GitHubOwner, repo, is); desc != nil {
			fields["description"] = desc
		}
	}

	// Set assignee based on GitHub issue/PR data
	if assigneeAccountID, err := determineJiraAssignee(ctx, cfg, repo, is); err != nil {
		log.Printf("warn: failed to determine assignee for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
	} else if assigneeAccountID != "" {
		fields["assignee"] = map[string]any{"accountId": assigneeAccountID}
	}

	// Project/issuetype references
	if cfg.UserConfig.JiraProjectID != "" {
		fields["project"] = map[string]any{"id": cfg.UserConfig.JiraProjectID}
	} else {
		fields["project"] = map[string]any{"key": cfg.UserConfig.JiraProjectKey}
	}
	if cfg.UserConfig.JiraIssueTypeID != "" {
		fields["issuetype"] = map[string]any{"id": cfg.UserConfig.JiraIssueTypeID}
	} else {
		fields["issuetype"] = map[string]any{"name": cfg.UserConfig.JiraIssueType}
	}

	// Enhance with required fields from CreateMeta
	reqFields, err := fetchCreateMetaRequiredFields(ctx, cfg)
	if err != nil {
		return fields, nil // proceed without meta if it fails
	}
	enhanceFieldsWithMeta(fields, reqFields)
	return fields, nil
}

func determineJiraComponents(userCfg UserConfig, repo string) []map[string]any {
	componentName := strings.TrimSpace(userCfg.JiraDefaultComponent)
	if userCfg.JiraComponents != nil {
		if mapped := strings.TrimSpace(userCfg.JiraComponents[repo]); mapped != "" {
			componentName = mapped
		}
	}
	if componentName == "" {
		componentName = "cert-manager"
	}
	return []map[string]any{{"name": componentName}}
}

var capacityCategoryByIssueTypeID = map[int]string{
	7830850: "Maintenance", // Task
	7830853: "Maintenance", // Bug
	7830856: "Feature",     // Feature
}

var capacityCategoryByIssueTypeName = map[string]string{
	"task":    "Maintenance",
	"bug":     "Maintenance",
	"feature": "Feature",
}

func determineCapacityCategory(is ghIssue) string {
	if is.IssueType == (ghIssueType{}) {
		return ""
	}
	if val, ok := capacityCategoryByIssueTypeID[is.IssueType.ID]; ok {
		return val
	}
	if name := strings.ToLower(strings.TrimSpace(is.IssueType.Name)); name != "" {
		if val, ok := capacityCategoryByIssueTypeName[name]; ok {
			return val
		}
	}
	return ""
}

// fetchCreateMetaRequiredFields fetches required fields for the configured project and issuetype.
func fetchCreateMetaRequiredFields(ctx context.Context, cfg config) (map[string]jiraFieldMetaInfo, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	base := fmt.Sprintf("%s/rest/api/3/issue/createmeta", strings.TrimRight(cfg.JiraBaseURL, "/"))
	q := url.Values{}
	if cfg.UserConfig.JiraProjectID != "" {
		q.Set("projectIds", cfg.UserConfig.JiraProjectID)
	} else if cfg.UserConfig.JiraProjectKey != "" {
		q.Set("projectKeys", cfg.UserConfig.JiraProjectKey)
	}
	if cfg.UserConfig.JiraIssueTypeID != "" {
		q.Set("issuetypeIds", cfg.UserConfig.JiraIssueTypeID)
	} else if cfg.UserConfig.JiraIssueType != "" {
		q.Set("issuetypeNames", cfg.UserConfig.JiraIssueType)
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
			if (cfg.UserConfig.JiraIssueTypeID != "" && it.ID == cfg.UserConfig.JiraIssueTypeID) || strings.EqualFold(it.Name, cfg.UserConfig.JiraIssueType) || cfg.UserConfig.JiraIssueType == "" {
				return it.Fields, nil
			}
		}
	}
	return nil, fmt.Errorf("issuetype %q not found in createmeta", cfg.UserConfig.JiraIssueType)
}

// enhanceFieldsWithMeta adds minimal values for required fields not already present.
func enhanceFieldsWithMeta(fields map[string]any, meta map[string]jiraFieldMetaInfo) {
	skip := map[string]struct{}{
		"summary": {}, "project": {}, "issuetype": {}, "labels": {}, "description": {}, "assignee": {}, "components": {},
	}
	skip[jiraCapacityCategoryField] = struct{}{}
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

func buildJiraADFDescription(owner, repo string, is ghIssue) *jiraADFDoc {
	linkText := fmt.Sprintf("GitHub %s/%s#%d", owner, repo, is.Number)
	return &jiraADFDoc{
		Type:    "doc",
		Version: 1,
		Content: []jiraADFNode{paragraphWithLink(linkText, is.HTMLURL)},
	}
}

func buildEnvironmentADF(owner, repo string, number int, href string) *jiraADFDoc {
	ref := summaryPrefix(owner, repo, number)
	return &jiraADFDoc{
		Type:    "doc",
		Version: 1,
		Content: []jiraADFNode{
			{
				Type: "paragraph",
				Content: []jiraADFNode{
					{Type: "text", Text: "Ref: "},
					{Type: "text", Text: ref, Marks: []jiraADFMark{{Type: "link", Attrs: map[string]any{"href": href}}}},
					{Type: "text", Text: " (do not edit this)"},
				},
			},
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

func summaryPrefix(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

func buildSummary(repo string, number int, title string) string {
	prefix := fmt.Sprintf("%s#%d: ", repo, number)
	remaining := 200 - len(prefix)
	if remaining < 0 {
		remaining = 0
	}
	return prefix + truncate(title, remaining)
}

func escapeJQLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
