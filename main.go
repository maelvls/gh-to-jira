package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-yaml/yaml"
)

const (
	jiraCapacityCategoryField = "customfield_10412"
	jiraSprintFieldKey        = "customfield_10020"
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

// UserConfig represents the YAML configuration for user mappings and application settings
type UserConfig struct {
	// GitHub configuration
	GitHubOwner string   `yaml:"github_owner"`
	GitHubRepo  string   `yaml:"github_repo"`
	GitHubRepos []string `yaml:"github_repos"`
	GitHubLabel string   `yaml:"github_label"`
	SyncPeriod  string   `yaml:"sync_period"`

	// Jira project configuration
	JiraProjectKey      string `yaml:"jira_project_key"`
	JiraProjectID       string `yaml:"jira_project_id"`
	JiraIssueType       string `yaml:"jira_issue_type"`
	JiraIssueTypeID     string `yaml:"jira_issue_type_id"`
	JiraSkipDescription bool   `yaml:"jira_skip_description"`

	// Jira status mapping
	JiraStatusOpen       string `yaml:"jira_status_open"`
	JiraStatusClosed     string `yaml:"jira_status_closed"`
	JiraStatusInProgress string `yaml:"jira_status_in_progress"`
	JiraStatusDraft      string `yaml:"jira_status_draft"`
	JiraStatusReopened   string `yaml:"jira_status_reopened"`

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

type jiraSprint struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state,omitzero"`
	StartDate string `json:"startDate,omitzero"`
	EndDate   string `json:"endDate,omitzero"`
}

type jiraTeamIteration struct {
	ID        int    `json:"id"`
	SprintID  int    `json:"sprintId,omitzero"`
	Name      string `json:"name"`
	State     string `json:"state"`
	StartDate string `json:"startDate,omitzero"`
	EndDate   string `json:"endDate,omitzero"`
	BoardID   int    `json:"boardId,omitzero"`
}

type jiraVersion struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Released    bool   `json:"released"`
	Archived    bool   `json:"archived"`
	Sequence    int    `json:"sequence,omitzero"`
	StartDate   string `json:"startDate,omitzero"`
	ReleaseDate string `json:"releaseDate,omitzero"`
}

type jiraBoard struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type,omitzero"`
}

type jiraBoardListResponse struct {
	Values     []jiraBoard `json:"values"`
	IsLast     bool        `json:"isLast"`
	StartAt    int         `json:"startAt,omitzero"`
	MaxResults int         `json:"maxResults,omitzero"`
}

type jiraSprintListResponse struct {
	Values     []jiraSprint `json:"values"`
	IsLast     bool         `json:"isLast"`
	StartAt    int          `json:"startAt,omitzero"`
	MaxResults int          `json:"maxResults,omitzero"`
}

type jiraComponent struct {
	ID   string `json:"id,omitzero"`
	Name string `json:"name,omitzero"`
}

type jiraUserRef struct {
	AccountID   string `json:"accountId,omitzero"`
	DisplayName string `json:"displayName,omitzero"`
}

type jiraIssueSyncFields struct {
	Labels              []string
	Sprints             []jiraSprint
	FixVersions         []jiraVersion
	Environment         *jiraADFDoc
	Components          []jiraComponent
	TeamOptionID        string
	AssigneeAccountID   string
	AssigneeDisplayName string
}

var targetSprintCache struct {
	mu      sync.Mutex
	sprint  jiraSprint
	err     error
	expires time.Time
}

var fixVersionCache struct {
	mu      sync.Mutex
	version jiraVersion
	err     error
	expires time.Time
}

var (
	errSprintNotFound     = errors.New("target sprint not found")
	errFixVersionNotFound = errors.New("target fix version not found")
)

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

	ticker := time.NewTicker(cfg.SyncPeriod)
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
			existingIssue, err := jiraFindExistingWithStatus(ctx, cfg, repo, is)
			if err != nil {
				log.Printf("warn: jira search failed for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
				continue
			}
			if existingIssue != nil {
				if cfg.DryRun {
					currentStatus := existingIssue.Fields.Status.Name
					desiredStatus := getDesiredJiraStatus(cfg, is, currentStatus)
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
					} else {
						statusMsg = fmt.Sprintf(" (status: keeping %q)", currentStatus)
					}
					assigneeInfo := ""
					if assigneeAccountID, clearAssignee, err := determineJiraAssignee(ctx, cfg, repo, is); err != nil {
						assigneeInfo = " (assignee determination failed)"
					} else if clearAssignee {
						assigneeInfo = " (would clear assignee)"
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
				if assigneeAccountID, clearAssignee, err := determineJiraAssignee(ctx, cfg, repo, is); err != nil {
					assigneeInfo = " (assignee determination failed)"
				} else if clearAssignee {
					assigneeInfo = " (would clear assignee)"
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
			alignWithTeamPlan := hasGitHubAssignee(is) || !isGitHubOpen(is)
			if aligned, err := ensureIssueInCurrentSprint(ctx, cfg, key, nil, alignWithTeamPlan); err != nil {
				log.Printf("warn: assigning sprint to %s failed: %v", key, err)
			} else if aligned {
				// Alignment already logged inside ensureIssueInCurrentSprint.
			}

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
	SyncPeriod  time.Duration
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
		SyncPeriod:   time.Minute,
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
		GitHubOwner:          "cert-manager",
		GitHubLabel:          "cybr",
		JiraIssueType:        "Task",
		JiraStatusOpen:       "To Do",
		JiraStatusClosed:     "Done",
		JiraStatusInProgress: "In Progress",
		JiraStatusDraft:      "In Progress",
		JiraStatusReopened:   "Reopened",
		JiraResolution:       "Done",
		GitHubToJiraUsers:    make(map[string]string),
		CyberArkKnownUsers:   []string{},
		JiraSkipDescription:  true,
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
	if userConfig.JiraStatusInProgress != "" {
		cfg.UserConfig.JiraStatusInProgress = userConfig.JiraStatusInProgress
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
	if userConfig.SyncPeriod != "" {
		period, err := time.ParseDuration(userConfig.SyncPeriod)
		if err != nil {
			return fmt.Errorf("invalid sync_period %q: %v", userConfig.SyncPeriod, err)
		}
		if period <= 0 {
			return fmt.Errorf("invalid sync_period %q: must be greater than zero", userConfig.SyncPeriod)
		}
		cfg.SyncPeriod = period
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
	if err := json.UnmarshalRead(resp.Body, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// fetchGitHubPRReviewers fetches the reviewers for a PR
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
	if err := json.UnmarshalRead(resp.Body, &issue); err != nil {
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

func extractJiraKeyFromBacklink(body string) string {
	if !strings.Contains(body, githubJiraBacklinkMarker) {
		return ""
	}
	const prefix = "CyberArk tracker:"
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, githubJiraBacklinkMarker) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		start := strings.Index(trimmed, prefix)
		if start == -1 {
			continue
		}
		segment := strings.TrimSpace(trimmed[start+len(prefix):])
		if !strings.HasPrefix(segment, "[") {
			continue
		}
		closing := strings.Index(segment, "]")
		if closing <= 1 {
			continue
		}
		return strings.TrimSpace(segment[1:closing])
	}
	return ""
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

// hasGitHubAssignee reports whether the GitHub item has at least one assignee.
func hasGitHubAssignee(is ghIssue) bool {
	if strings.TrimSpace(is.Assignee.Login) != "" {
		return true
	}
	for _, assignee := range is.Assignees {
		if strings.TrimSpace(assignee.Login) != "" {
			return true
		}
	}
	return false
}

func isGitHubOpen(is ghIssue) bool {
	if is.PullRequest != nil {
		if is.Merged {
			return false
		}
		return strings.EqualFold(strings.TrimSpace(is.State), "open")
	}
	return strings.EqualFold(strings.TrimSpace(is.State), "open")
}

func isNewStatus(status string, cfg config) bool {
	status = strings.TrimSpace(status)
	if status == "" {
		return true
	}
	if statusNamesMatch(status, cfg.UserConfig.JiraStatusOpen) {
		return true
	}
	return strings.EqualFold(status, "new")
}

// determineJiraAssignee determines the best Jira assignee based on GitHub issue/PR data.
// Returns a Jira account ID, a flag indicating whether Jira should be explicitly unassigned, and an error.
func determineJiraAssignee(_ context.Context, cfg config, _ string, is ghIssue) (string, bool, error) {
	if !hasGitHubAssignee(is) {
		return "", true, nil
	}

	seen := make(map[string]struct{})
	var candidates []string

	if login := strings.TrimSpace(is.Assignee.Login); login != "" {
		candidates = append(candidates, login)
		seen[login] = struct{}{}
	}

	for _, assignee := range is.Assignees {
		login := strings.TrimSpace(assignee.Login)
		if login == "" {
			continue
		}
		if _, already := seen[login]; already {
			continue
		}
		seen[login] = struct{}{}
		candidates = append(candidates, login)
	}

	for _, login := range candidates {
		if accountID, ok := cfg.UserConfig.GitHubToJiraUsers[login]; ok && strings.TrimSpace(accountID) != "" {
			return strings.TrimSpace(accountID), false, nil
		}
		if accountID, ok := cfg.UserConfig.GitHubToJiraUsers[strings.ToLower(login)]; ok && strings.TrimSpace(accountID) != "" {
			return strings.TrimSpace(accountID), false, nil
		}
	}

	return "", false, nil // No suitable assignee found.
}

// milestone logic removed

// Jira helper: basic auth header
func jiraAuthHeader(email, token string) string {
	raw := email + ":" + token
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func jiraGetBasicIssue(ctx context.Context, cfg config, issueKey string) (*jiraBasicIssue, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=status", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(issueKey))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		bdy, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("jira get issue %s status %d: %s", issueKey, resp.StatusCode, string(bdy))
	}
	var issue jiraBasicIssue
	if err := json.UnmarshalRead(resp.Body, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

func jiraFindExisting(ctx context.Context, cfg config, repo string, is ghIssue) (string, error) {
	if key := extractJiraKeyFromBacklink(is.Body); key != "" {
		return key, nil
	}
	return jiraFindExistingByEnvironment(ctx, cfg, repo, is.Number)
}

func jiraFindExistingByEnvironment(ctx context.Context, cfg config, repo string, ghNumber int) (string, error) {
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
			if err := json.UnmarshalRead(resp.Body, &out); err == nil {
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
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
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
				if err := json.UnmarshalRead(resp2.Body, &out); err != nil {
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
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
		return "", err
	}
	return out.Key, nil
}

func jiraFindExistingWithStatus(ctx context.Context, cfg config, repo string, is ghIssue) (*jiraBasicIssue, error) {
	if key := extractJiraKeyFromBacklink(is.Body); key != "" {
		issue, err := jiraGetBasicIssue(ctx, cfg, key)
		if err != nil {
			return nil, err
		}
		if issue != nil {
			return issue, nil
		}
		return jiraFindExistingWithStatusByEnvironment(ctx, cfg, repo, is.Number)
	}
	return jiraFindExistingWithStatusByEnvironment(ctx, cfg, repo, is.Number)
}

func jiraFindExistingWithStatusByEnvironment(ctx context.Context, cfg config, repo string, ghNumber int) (*jiraBasicIssue, error) {
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
			if err := json.UnmarshalRead(resp.Body, &out); err == nil {
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
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
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
	repoRef := fmt.Sprintf("%s/%s#%d", cfg.UserConfig.GitHubOwner, repo, is.Number)
	client := &http.Client{Timeout: cfg.HTTPTimeout}

	// Merge labels with existing to avoid losing data.
	snapshot, err := jiraGetIssueSyncFields(ctx, cfg, issueKey)
	if err != nil {
		log.Printf("warn: failed to load labels and sprint for %s: %v", issueKey, err)
	}
	hasAssignee := hasGitHubAssignee(is)
	alignWithTeamPlan := hasAssignee || !isGitHubOpen(is)
	desired := uniqueStrings(append(snapshot.Labels, []string{"OpenSource", "gh-to-jira", fmt.Sprintf("repo:%s", repo)}...))
	envDoc := buildEnvironmentADF(cfg.UserConfig.GitHubOwner, repo, is.Number, is.HTMLURL)
	components := determineJiraComponents(cfg.UserConfig, repo)
	// We no longer force sprint updates here so Jira users can control the field after creation.
	fieldPayload := map[string]any{
		"labels":      desired,
		"environment": envDoc,
	}
	if len(components) > 0 {
		fieldPayload["components"] = components
	}

	// Ensure team remains set on updates as well, if configured.
	teamFieldKey := strings.TrimSpace(cfg.UserConfig.JiraTeamFieldKey)
	teamOptionID := strings.TrimSpace(cfg.UserConfig.JiraTeamOptionID)
	if teamFieldKey != "" && teamOptionID != "" {
		fieldPayload[teamFieldKey] = map[string]any{"id": teamOptionID}
	}
	// Capacity category remains untouched during updates so teams can adjust it in Jira.

	var (
		newAssigneeAccountID string
		shouldClearAssignee  bool
		assignDecisionMade   bool
	)

	// Update assignee based on GitHub issue/PR data.
	if assigneeAccountID, clearAssignee, determineErr := determineJiraAssignee(ctx, cfg, repo, is); determineErr != nil {
		log.Printf("warn: failed to determine assignee for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, determineErr)
	} else {
		newAssigneeAccountID = strings.TrimSpace(assigneeAccountID)
		shouldClearAssignee = clearAssignee
		assignDecisionMade = shouldClearAssignee || newAssigneeAccountID != ""
		if shouldClearAssignee {
			fieldPayload["assignee"] = nil
		} else if newAssigneeAccountID != "" {
			fieldPayload["assignee"] = map[string]any{"accountId": newAssigneeAccountID}
		}
	}

	var fixVersionForUpdate *jiraVersion
	if alignWithTeamPlan {
		if fixVersion, resolveErr := resolveCurrentFixVersion(ctx, cfg); resolveErr != nil {
			if !errors.Is(resolveErr, errFixVersionNotFound) {
				log.Printf("warn: failed to resolve fix version for %s: %v", repoRef, resolveErr)
			}
		} else if strings.TrimSpace(fixVersion.ID) != "" {
			if len(snapshot.FixVersions) != 1 || !issueHasFixVersion(snapshot.FixVersions, fixVersion.ID) {
				copyFixVersion := fixVersion
				fixVersionForUpdate = &copyFixVersion
				fieldPayload["fixVersions"] = []map[string]any{{"id": fixVersion.ID}}
			}
		}
	}

	type fieldChange struct {
		Field  string
		Reason string
		Old    string
		New    string
	}
	var changes []fieldChange

	if !stringSlicesEqualIgnoreOrder(snapshot.Labels, desired) {
		changes = append(changes, fieldChange{
			Field:  "labels",
			Reason: fmt.Sprintf("Keeping Jira labels aligned with GitHub %s.", repoRef),
			Old:    formatLogValue(sortedStrings(snapshot.Labels)),
			New:    formatLogValue(sortedStrings(desired)),
		})
	}

	if !jiraADFDocsEqual(snapshot.Environment, envDoc) {
		changes = append(changes, fieldChange{
			Field:  "environment",
			Reason: fmt.Sprintf("Refreshing environment details from GitHub %s.", repoRef),
			Old:    formatLogValue(snapshot.Environment),
			New:    formatLogValue(envDoc),
		})
	}

	if len(components) > 0 {
		oldComponentNames := componentNamesFromSnapshot(snapshot.Components)
		newComponentNames := componentNamesFromConfig(components)
		if !stringSlicesEqualIgnoreOrder(oldComponentNames, newComponentNames) {
			changes = append(changes, fieldChange{
				Field:  "components",
				Reason: fmt.Sprintf("Enforcing repository component mapping for %s.", repoRef),
				Old:    formatLogValue(sortedStrings(oldComponentNames)),
				New:    formatLogValue(sortedStrings(newComponentNames)),
			})
		}
	}

	if teamFieldKey != "" && teamOptionID != "" {
		if !strings.EqualFold(strings.TrimSpace(snapshot.TeamOptionID), teamOptionID) {
			changes = append(changes, fieldChange{
				Field:  teamFieldKey,
				Reason: fmt.Sprintf("Applying configured team option for %s.", repoRef),
				Old:    formatLogValue(strings.TrimSpace(snapshot.TeamOptionID)),
				New:    formatLogValue(teamOptionID),
			})
		}
	}

	if fixVersionForUpdate != nil {
		changes = append(changes, fieldChange{
			Field:  "fixVersions",
			Reason: fmt.Sprintf("Aligning fix version with current program increment for %s.", repoRef),
			Old:    formatLogValue(sortedStrings(fixVersionNames(snapshot.FixVersions))),
			New:    formatLogValue([]string{strings.TrimSpace(fixVersionForUpdate.Name)}),
		})
	}

	if assignDecisionMade {
		oldAssigneeID := strings.TrimSpace(snapshot.AssigneeAccountID)
		newAssigneeID := newAssigneeAccountID
		if shouldClearAssignee {
			newAssigneeID = ""
		}
		if oldAssigneeID != newAssigneeID {
			reason := fmt.Sprintf("Aligning Jira assignee with GitHub %s.", repoRef)
			if shouldClearAssignee {
				reason = fmt.Sprintf("Clearing Jira assignee to mirror GitHub %s.", repoRef)
			}
			oldValue := assigneeLogValue(snapshot.AssigneeAccountID, snapshot.AssigneeDisplayName)
			var newValue any
			if newAssigneeID == "" {
				newValue = nil
			} else {
				newValue = map[string]string{"accountId": newAssigneeID}
			}
			changes = append(changes, fieldChange{
				Field:  "assignee",
				Reason: reason,
				Old:    formatLogValue(oldValue),
				New:    formatLogValue(newValue),
			})
		}
	}

	var sprintAligned bool
	if alignWithTeamPlan {
		if aligned, sprintErr := ensureIssueInCurrentSprint(ctx, cfg, issueKey, snapshot.Sprints, alignWithTeamPlan); sprintErr != nil {
			log.Printf("warn: aligning sprint for %s failed: %v", issueKey, sprintErr)
		} else {
			sprintAligned = aligned
		}
	}

	if len(changes) == 0 {
		if !sprintAligned {
			log.Printf("Jira issue %s already reflects GitHub %s; no field changes required.", issueKey, repoRef)
		}
		return nil
	}

	payload := jiraIssueCreateRequest{Fields: fieldPayload}

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
		for _, change := range changes {
			log.Printf("Jira %s update (%s): field=%s old=%s new=%s", issueKey, change.Reason, change.Field, change.Old, change.New)
		}
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

	// Get current status
	currentStatus := jiraIssue.Fields.Status.Name
	// Determine desired Jira status based on GitHub state
	desiredStatus := getDesiredJiraStatus(cfg, is, currentStatus)
	if desiredStatus == "" {
		return nil // No status change needed
	}
	repoRef := fmt.Sprintf("%s/%s#%d", cfg.UserConfig.GitHubOwner, repo, is.Number)
	githubState := is.State
	if is.PullRequest != nil {
		if is.Merged {
			githubState = "merged"
		} else if is.Draft {
			githubState = "draft"
		}
	}

	// Handle the "NOT_CLOSED" rule for open GitHub issues/PRs
	if desiredStatus == "NOT_CLOSED" {
		// For open GitHub items, we want any status except the closed status
		if isClosedStatus(currentStatus) {
			// Current status is "Done/Closed" but GitHub is open - need to reopen
			reason := fmt.Sprintf("GitHub %s reopened; restoring workflow alignment.", repoRef)
			log.Printf("Preparing Jira %s status update (%s): old=%q target=%q", jiraIssue.Key, reason, currentStatus, cfg.UserConfig.JiraStatusReopened)
			return jiraTransitionStatus(ctx, cfg, jiraIssue.Key, currentStatus, cfg.UserConfig.JiraStatusReopened, reason)
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
	reason := fmt.Sprintf("GitHub %s is %q; syncing Jira workflow.", repoRef, githubState)
	log.Printf("Preparing Jira %s status update (%s): old=%q target=%q", jiraIssue.Key, reason, currentStatus, desiredStatus)

	// Perform status transition
	return jiraTransitionStatus(ctx, cfg, jiraIssue.Key, currentStatus, desiredStatus, reason)
}

// getDesiredJiraStatus maps GitHub issue/PR state to desired Jira status
func getDesiredJiraStatus(cfg config, is ghIssue, currentStatus string) string {
	hasAssignee := hasGitHubAssignee(is)
	inProgressStatus := strings.TrimSpace(cfg.UserConfig.JiraStatusInProgress)
	if inProgressStatus == "" {
		inProgressStatus = strings.TrimSpace(cfg.UserConfig.JiraStatusDraft)
	}
	currentStatus = strings.TrimSpace(currentStatus)
	assignedAndOpen := hasAssignee && isGitHubOpen(is)

	if is.PullRequest != nil {
		// This is a PR
		if is.Merged {
			return cfg.UserConfig.JiraStatusClosed
		}
		if is.Draft {
			if hasAssignee && inProgressStatus != "" {
				return inProgressStatus
			}
			return cfg.UserConfig.JiraStatusDraft
		}
		if is.State == "closed" {
			return cfg.UserConfig.JiraStatusClosed
		}
	} else {
		// This is an issue
		if is.State == "closed" {
			return cfg.UserConfig.JiraStatusClosed
		}
	}

	if assignedAndOpen && inProgressStatus != "" {
		if currentStatus == "" || isNewStatus(currentStatus, cfg) || isClosedStatus(currentStatus) {
			return inProgressStatus
		}
		return ""
	}

	if isGitHubOpen(is) {
		// For open GitHub items, any status except closed/done is acceptable.
		return "NOT_CLOSED" // Special marker meaning "any status except closed".
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

// jiraTransitionStatus transitions a Jira issue to the specified status.
func jiraTransitionStatus(ctx context.Context, cfg config, issueKey, currentStatus, targetStatus, reason string) error {
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
		finalStatus := targetStatus
		if matchedStatusName != "" && !strings.EqualFold(matchedStatusName, targetStatus) {
			finalStatus = matchedStatusName
		}
		log.Printf("Jira %s status update (%s): old=%q new=%q", issueKey, reason, currentStatus, finalStatus)
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
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
		return nil, err
	}
	return out.Transitions, nil
}

func jiraGetIssueSyncFields(ctx context.Context, cfg config, issueKey string) (jiraIssueSyncFields, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	fieldList := []string{"labels", jiraSprintFieldKey, "fixVersions", "environment", "components", "assignee"}
	if teamFieldKey := strings.TrimSpace(cfg.UserConfig.JiraTeamFieldKey); teamFieldKey != "" {
		fieldList = append(fieldList, teamFieldKey)
	}
	fieldsParam := strings.Join(fieldList, ",")
	reqURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=%s", strings.TrimRight(cfg.JiraBaseURL, "/"), url.PathEscape(issueKey), url.QueryEscape(fieldsParam))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return jiraIssueSyncFields{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return jiraIssueSyncFields{}, fmt.Errorf("jira get issue sync fields status %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Fields map[string]jsontext.Value `json:"fields"`
	}
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
		return jiraIssueSyncFields{}, err
	}
	var snapshot jiraIssueSyncFields
	if labelsRaw, ok := out.Fields["labels"]; ok && len(labelsRaw) > 0 {
		var labels []string
		if err := json.Unmarshal([]byte(labelsRaw), &labels); err == nil {
			snapshot.Labels = labels
		}
	}
	if sprintsRaw, ok := out.Fields[jiraSprintFieldKey]; ok && len(sprintsRaw) > 0 {
		snapshot.Sprints = parseSprintField(sprintsRaw)
	}
	if fixVersionsRaw, ok := out.Fields["fixVersions"]; ok && len(fixVersionsRaw) > 0 {
		var fixVersions []jiraVersion
		if err := json.Unmarshal([]byte(fixVersionsRaw), &fixVersions); err == nil {
			snapshot.FixVersions = fixVersions
		}
	}
	if envRaw, ok := out.Fields["environment"]; ok && len(envRaw) > 0 {
		var env jiraADFDoc
		if err := json.Unmarshal([]byte(envRaw), &env); err == nil {
			snapshot.Environment = &env
		}
	}
	if componentsRaw, ok := out.Fields["components"]; ok && len(componentsRaw) > 0 {
		var components []jiraComponent
		if err := json.Unmarshal([]byte(componentsRaw), &components); err == nil {
			snapshot.Components = components
		}
	}
	if assigneeRaw, ok := out.Fields["assignee"]; ok && len(assigneeRaw) > 0 {
		var assignee jiraUserRef
		if err := json.Unmarshal([]byte(assigneeRaw), &assignee); err == nil {
			snapshot.AssigneeAccountID = strings.TrimSpace(assignee.AccountID)
			snapshot.AssigneeDisplayName = strings.TrimSpace(assignee.DisplayName)
		}
	}
	if teamFieldKey := strings.TrimSpace(cfg.UserConfig.JiraTeamFieldKey); teamFieldKey != "" {
		if teamRaw, ok := out.Fields[teamFieldKey]; ok && len(teamRaw) > 0 {
			var team struct {
				ID string `json:"id,omitzero"`
			}
			if err := json.Unmarshal([]byte(teamRaw), &team); err == nil && strings.TrimSpace(team.ID) != "" {
				snapshot.TeamOptionID = strings.TrimSpace(team.ID)
			} else {
				var id string
				if err := json.Unmarshal([]byte(teamRaw), &id); err == nil {
					snapshot.TeamOptionID = strings.TrimSpace(id)
				}
			}
		}
	}
	return snapshot, nil
}

func parseSprintField(raw jsontext.Value) []jiraSprint {
	if len(raw) == 0 {
		return nil
	}
	var sprints []jiraSprint
	if err := json.Unmarshal([]byte(raw), &sprints); err == nil {
		return sprints
	}
	var single jiraSprint
	if err := json.Unmarshal([]byte(raw), &single); err == nil {
		return []jiraSprint{single}
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err == nil {
		for _, name := range names {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				sprints = append(sprints, jiraSprint{Name: trimmed})
			}
		}
		return sprints
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			return []jiraSprint{{Name: trimmed}}
		}
	}
	return nil
}

func issueHasSprint(sprints []jiraSprint, target jiraSprint) bool {
	if target.ID <= 0 && strings.TrimSpace(target.Name) == "" {
		return false
	}
	for _, sprint := range sprints {
		if target.ID > 0 && sprint.ID == target.ID {
			return true
		}
	}
	targetName := strings.TrimSpace(target.Name)
	if targetName == "" {
		return false
	}
	for _, sprint := range sprints {
		if strings.EqualFold(strings.TrimSpace(sprint.Name), targetName) {
			return true
		}
	}
	return false
}

func ensureIssueInCurrentSprint(ctx context.Context, cfg config, issueKey string, currentSprints []jiraSprint, shouldAlign bool) (bool, error) {
	if !shouldAlign {
		return false, nil
	}
	sprint, err := resolveCurrentSprint(ctx, cfg)
	if err != nil {
		if errors.Is(err, errSprintNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("resolve sprint: %w", err)
	}
	if sprint.ID <= 0 {
		return false, nil
	}
	if issueHasSprint(currentSprints, sprint) {
		return false, nil
	}
	if err := jiraAddIssuesToSprint(ctx, cfg, sprint.ID, []string{issueKey}); err != nil {
		return false, fmt.Errorf("assign issue %s to sprint %d: %w", issueKey, sprint.ID, err)
	}
	log.Printf("Aligned Jira issue %s with sprint %q (id=%d).", issueKey, sprint.Name, sprint.ID)
	return true, nil
}

func resolveCurrentSprint(ctx context.Context, cfg config) (jiraSprint, error) {
	now := time.Now()
	targetSprintCache.mu.Lock()
	if now.Before(targetSprintCache.expires) {
		sprint := targetSprintCache.sprint
		err := targetSprintCache.err
		targetSprintCache.mu.Unlock()
		if err != nil {
			return jiraSprint{}, err
		}
		if sprint.ID == 0 {
			return jiraSprint{}, errSprintNotFound
		}
		return sprint, nil
	}
	targetSprintCache.mu.Unlock()

	sprint, err := fetchCurrentSprint(ctx, cfg)
	expiry := time.Now().Add(30 * time.Second)
	targetSprintCache.mu.Lock()
	if err == nil {
		targetSprintCache.sprint = sprint
	}
	targetSprintCache.err = err
	targetSprintCache.expires = expiry
	targetSprintCache.mu.Unlock()
	if err != nil {
		return jiraSprint{}, err
	}
	if sprint.ID == 0 {
		return jiraSprint{}, errSprintNotFound
	}
	return sprint, nil
}

func fetchCurrentSprint(ctx context.Context, cfg config) (jiraSprint, error) {
	teamID := strings.TrimSpace(cfg.UserConfig.JiraTeamOptionID)
	if teamID == "" {
		return jiraSprint{}, errSprintNotFound
	}
	sprint, err := jiraFindTeamSprint(ctx, cfg, teamID)
	if err != nil {
		return jiraSprint{}, err
	}
	return sprint, nil
}

func jiraFindTeamSprint(ctx context.Context, cfg config, teamID string) (jiraSprint, error) {
	active, err := jiraListTeamIterations(ctx, cfg, teamID, "ACTIVE")
	if err != nil {
		return jiraSprint{}, err
	}
	if len(active) > 0 {
		selected := selectIteration(active, true)
		return iterationToSprint(selected), nil
	}
	future, err := jiraListTeamIterations(ctx, cfg, teamID, "FUTURE")
	if err != nil {
		return jiraSprint{}, err
	}
	if len(future) > 0 {
		selected := selectIteration(future, false)
		return iterationToSprint(selected), nil
	}
	return jiraSprint{}, errSprintNotFound
}

func jiraListTeamIterations(ctx context.Context, cfg config, teamID, state string) ([]jiraTeamIteration, error) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	base := strings.TrimRight(cfg.JiraBaseURL, "/")
	startAt := 0
	var iterations []jiraTeamIteration
	for {
		reqURL := fmt.Sprintf("%s/rest/teams/1.0/teams/%s/iterations?state=%s&startAt=%d&maxResults=50", base, url.PathEscape(teamID), url.QueryEscape(state), startAt)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, fmt.Errorf("jira team iterations status %d: %s", resp.StatusCode, string(b))
		}
		var out struct {
			Values     []jiraTeamIteration `json:"values"`
			IsLast     bool                `json:"isLast,omitzero"`
			MaxResults int                 `json:"maxResults,omitzero"`
		}
		if decodeErr := json.UnmarshalRead(resp.Body, &out); decodeErr != nil {
			resp.Body.Close()
			return nil, decodeErr
		}
		resp.Body.Close()
		iterations = append(iterations, out.Values...)
		if out.IsLast || len(out.Values) == 0 || len(out.Values) < out.MaxResults {
			break
		}
		startAt += len(out.Values)
	}
	return iterations, nil
}

func selectIteration(iterations []jiraTeamIteration, preferLatest bool) jiraTeamIteration {
	if len(iterations) == 0 {
		return jiraTeamIteration{}
	}
	var (
		chosen        jiraTeamIteration
		chosenTime    time.Time
		chosenHasTime bool
		set           bool
	)
	for _, iter := range iterations {
		iterTime, hasTime := parseJiraDateTime(iter.StartDate)
		if !set {
			chosen = iter
			chosenTime = iterTime
			chosenHasTime = hasTime
			set = true
			continue
		}
		switch {
		case chosenHasTime && hasTime:
			if preferLatest {
				if iterTime.After(chosenTime) || iterTime.Equal(chosenTime) && iter.ID > chosen.ID {
					chosen = iter
					chosenTime = iterTime
				}
			} else {
				if iterTime.Before(chosenTime) || iterTime.Equal(chosenTime) && iter.ID < chosen.ID {
					chosen = iter
					chosenTime = iterTime
				}
			}
		case hasTime && !chosenHasTime:
			chosen = iter
			chosenTime = iterTime
			chosenHasTime = true
		case !hasTime && !chosenHasTime:
			if preferLatest {
				if iter.ID > chosen.ID {
					chosen = iter
				}
			} else {
				if iter.ID < chosen.ID {
					chosen = iter
				}
			}
		}
	}
	return chosen
}

func iterationToSprint(iter jiraTeamIteration) jiraSprint {
	sprintID := iter.SprintID
	if sprintID == 0 {
		sprintID = iter.ID
	}
	return jiraSprint{
		ID:        sprintID,
		Name:      iter.Name,
		State:     iter.State,
		StartDate: iter.StartDate,
		EndDate:   iter.EndDate,
	}
}

func parseJiraDateTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05.000Z0700",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if ts, err := time.Parse(layout, value); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

func resolveCurrentFixVersion(ctx context.Context, cfg config) (jiraVersion, error) {
	now := time.Now()
	fixVersionCache.mu.Lock()
	if now.Before(fixVersionCache.expires) {
		version := fixVersionCache.version
		err := fixVersionCache.err
		fixVersionCache.mu.Unlock()
		if err != nil {
			return jiraVersion{}, err
		}
		if strings.TrimSpace(version.ID) == "" {
			return jiraVersion{}, errFixVersionNotFound
		}
		return version, nil
	}
	fixVersionCache.mu.Unlock()

	version, err := fetchCurrentFixVersion(ctx, cfg)
	expiry := time.Now().Add(2 * time.Minute)
	fixVersionCache.mu.Lock()
	if err == nil {
		fixVersionCache.version = version
	}
	fixVersionCache.err = err
	fixVersionCache.expires = expiry
	fixVersionCache.mu.Unlock()
	if err != nil {
		return jiraVersion{}, err
	}
	if strings.TrimSpace(version.ID) == "" {
		return jiraVersion{}, errFixVersionNotFound
	}
	return version, nil
}

func fetchCurrentFixVersion(ctx context.Context, cfg config) (jiraVersion, error) {
	versions, err := jiraListProjectVersions(ctx, cfg)
	if err != nil {
		return jiraVersion{}, err
	}
	if len(versions) == 0 {
		return jiraVersion{}, errFixVersionNotFound
	}
	if version, ok := selectCurrentFixVersion(versions); ok {
		return version, nil
	}
	return jiraVersion{}, errFixVersionNotFound
}

func jiraListProjectVersions(ctx context.Context, cfg config) ([]jiraVersion, error) {
	projectKeyOrID := strings.TrimSpace(cfg.UserConfig.JiraProjectID)
	if projectKeyOrID == "" {
		projectKeyOrID = strings.TrimSpace(cfg.UserConfig.JiraProjectKey)
	}
	if projectKeyOrID == "" {
		return nil, fmt.Errorf("missing Jira project key or id for version lookup")
	}
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	base := strings.TrimRight(cfg.JiraBaseURL, "/")
	startAt := 0
	var versions []jiraVersion
	for {
		reqURL := fmt.Sprintf("%s/rest/api/3/project/%s/versions?maxResults=50&orderBy=-sequence&startAt=%d", base, url.PathEscape(projectKeyOrID), startAt)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		req.Header.Set("Authorization", jiraAuthHeader(cfg.JiraEmail, cfg.JiraAPIToken))
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("jira version list status %d: %s", resp.StatusCode, string(body))
		}
		var wrapper struct {
			Values     []jiraVersion `json:"values"`
			IsLast     bool          `json:"isLast,omitzero"`
			MaxResults int           `json:"maxResults,omitzero"`
		}
		if err := json.Unmarshal(body, &wrapper); err == nil && (len(wrapper.Values) > 0 || wrapper.IsLast) {
			versions = append(versions, wrapper.Values...)
			if wrapper.IsLast || len(wrapper.Values) == 0 || len(wrapper.Values) < wrapper.MaxResults {
				break
			}
			startAt += len(wrapper.Values)
			continue
		}
		var direct []jiraVersion
		if err := json.Unmarshal(body, &direct); err == nil {
			versions = append(versions, direct...)
			break
		}
		return nil, fmt.Errorf("unexpected Jira version payload: %s", string(body))
	}
	return versions, nil
}

func selectCurrentFixVersion(versions []jiraVersion) (jiraVersion, bool) {
	var chosen jiraVersion
	found := false
	for _, v := range versions {
		if v.Archived || v.Released {
			continue
		}
		if !found {
			chosen = v
			found = true
			continue
		}
		if compareVersions(v, chosen) > 0 {
			chosen = v
		}
	}
	if found {
		return chosen, true
	}
	for _, v := range versions {
		if v.Archived {
			continue
		}
		if !found {
			chosen = v
			found = true
			continue
		}
		if compareVersions(v, chosen) > 0 {
			chosen = v
		}
	}
	return chosen, found
}

func compareVersions(a, b jiraVersion) int {
	if a.Sequence != 0 || b.Sequence != 0 {
		if a.Sequence > b.Sequence {
			return 1
		}
		if a.Sequence < b.Sequence {
			return -1
		}
	}
	if ta, oka := parseJiraDateTime(a.ReleaseDate); oka {
		if tb, okb := parseJiraDateTime(b.ReleaseDate); okb {
			if ta.After(tb) {
				return 1
			}
			if ta.Before(tb) {
				return -1
			}
		} else {
			return 1
		}
	} else if _, okb := parseJiraDateTime(b.ReleaseDate); okb {
		return -1
	}
	if ta, oka := parseJiraDateTime(a.StartDate); oka {
		if tb, okb := parseJiraDateTime(b.StartDate); okb {
			if ta.After(tb) {
				return 1
			}
			if ta.Before(tb) {
				return -1
			}
		} else {
			return 1
		}
	} else if _, okb := parseJiraDateTime(b.StartDate); okb {
		return -1
	}
	return strings.Compare(strings.TrimSpace(a.Name), strings.TrimSpace(b.Name))
}

func issueHasFixVersion(existing []jiraVersion, targetID string) bool {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return false
	}
	for _, version := range existing {
		if strings.TrimSpace(version.ID) == targetID {
			return true
		}
	}
	return false
}

func fixVersionNames(versions []jiraVersion) []string {
	names := make([]string, 0, len(versions))
	for _, version := range versions {
		if trimmed := strings.TrimSpace(version.Name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

func jiraAddIssuesToSprint(ctx context.Context, cfg config, sprintID int, issueKeys []string) error {
	if sprintID <= 0 {
		return fmt.Errorf("invalid sprint id %d", sprintID)
	}
	if len(issueKeys) == 0 {
		return nil
	}
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	payload := map[string]any{"issues": issueKeys}
	body, _ := json.Marshal(payload)
	reqURL := fmt.Sprintf("%s/rest/agile/1.0/sprint/%d/issue", strings.TrimRight(cfg.JiraBaseURL, "/"), sprintID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(string(body)))
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
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("jira sprint assignment status %d: %s", resp.StatusCode, string(b))
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
	hasAssignee := hasGitHubAssignee(is)
	alignWithTeamPlan := hasAssignee || !isGitHubOpen(is)
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
	if assigneeAccountID, clearAssignee, err := determineJiraAssignee(ctx, cfg, repo, is); err != nil {
		log.Printf("warn: failed to determine assignee for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, err)
	} else if clearAssignee {
		fields["assignee"] = nil
	} else if assigneeAccountID != "" {
		fields["assignee"] = map[string]any{"accountId": assigneeAccountID}
	}

	if alignWithTeamPlan {
		if fixVersion, resolveErr := resolveCurrentFixVersion(ctx, cfg); resolveErr != nil {
			if !errors.Is(resolveErr, errFixVersionNotFound) {
				log.Printf("warn: failed to resolve fix version for %s/%s#%d: %v", cfg.UserConfig.GitHubOwner, repo, is.Number, resolveErr)
			}
		} else if strings.TrimSpace(fixVersion.ID) != "" {
			fields["fixVersions"] = []map[string]any{{"id": fixVersion.ID}}
		}
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

const defaultCapacityCategory = "Maintenance"

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
	// Default to Maintenance so freshly created tickets always get a value.
	if is.IssueType == (ghIssueType{}) {
		return defaultCapacityCategory
	}
	if val, ok := capacityCategoryByIssueTypeID[is.IssueType.ID]; ok {
		return val
	}
	if name := strings.ToLower(strings.TrimSpace(is.IssueType.Name)); name != "" {
		if val, ok := capacityCategoryByIssueTypeName[name]; ok {
			return val
		}
	}
	return defaultCapacityCategory
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
	if err := json.UnmarshalRead(resp.Body, &meta); err != nil {
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
	skip[jiraSprintFieldKey] = struct{}{}
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

func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func stringSlicesEqualIgnoreOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[strings.TrimSpace(v)]++
	}
	for _, v := range b {
		key := strings.TrimSpace(v)
		if count, ok := counts[key]; !ok || count == 0 {
			return false
		} else {
			if count == 1 {
				delete(counts, key)
			} else {
				counts[key] = count - 1
			}
		}
	}
	return len(counts) == 0
}

func jiraADFDocsEqual(a, b *jiraADFDoc) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func componentNamesFromSnapshot(components []jiraComponent) []string {
	if len(components) == 0 {
		return []string{}
	}
	names := make([]string, 0, len(components))
	for _, component := range components {
		if name := strings.TrimSpace(component.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func componentNamesFromConfig(components []map[string]any) []string {
	if len(components) == 0 {
		return []string{}
	}
	names := make([]string, 0, len(components))
	for _, component := range components {
		if rawName, ok := component["name"]; ok {
			if name, ok := rawName.(string); ok {
				if trimmed := strings.TrimSpace(name); trimmed != "" {
					names = append(names, trimmed)
				}
			}
		}
	}
	return names
}

func formatLogValue(value any) string {
	if value == nil {
		return "null"
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(b)
}

func assigneeLogValue(accountID, displayName string) any {
	accountID = strings.TrimSpace(accountID)
	displayName = strings.TrimSpace(displayName)
	if accountID == "" && displayName == "" {
		return nil
	}
	value := map[string]string{}
	if accountID != "" {
		value["accountId"] = accountID
	}
	if displayName != "" {
		value["displayName"] = displayName
	}
	return value
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
