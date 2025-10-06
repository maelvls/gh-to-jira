gh-to-jira: Sync labelled cert-manager issues/PRs to Jira

Overview
- A small Go bot that creates/updates Jira tickets for GitHub issues and PRs across the `cert-manager` organisation carrying a specific label (default `cybr`), polling every minute.
- Uses GitHub REST API and Jira Cloud REST API v3 with remote link back to GitHub.
- Avoids duplicates by storing a Jira issue property that records the GitHub owner, repo, and issue/PR number.

Prerequisites
- Go 1.22+
- A GitHub token with `repo:read` scope in `GITHUB_TOKEN`.
- A Jira Cloud site and a project. Create a Jira API token and set the following env vars:
  - `JIRA_BASE_URL` (e.g., `https://your-domain.atlassian.net`)
  - `JIRA_EMAIL` (your Atlassian account email)
  - `JIRA_API_TOKEN` (API token)
- A `config.yaml` file with your project configuration (see Configuration File section below)

Environment Variables (Secrets Only)
The following **secret** environment variables are required:
- `GITHUB_TOKEN` (required) - GitHub token with repo access
- `JIRA_BASE_URL` (required) - Your Jira instance URL
- `JIRA_EMAIL` (required) - Your Atlassian account email  
- `JIRA_API_TOKEN` (required) - Your Jira API token
- `CONFIG_PATH` (optional) - Path to config file (default: `config.yaml`)

**All other configuration has been moved to the config.yaml file.** Environment variables can still override YAML settings for backwards compatibility.

Configuration File
The bot uses a YAML configuration file (default: `config.yaml`) for all non-secret settings. This includes GitHub repository settings, Jira project configuration, status mappings, and user mappings.

**Minimal `config.yaml` example:**
```yaml
# Required settings
jira_project_key: "PROJ"             # Your Jira project key

# GitHub settings (defaults shown)
github_owner: "cert-manager"         # GitHub org/user
github_label: "cybr"                 # Label to filter issues/PRs

# User mappings
github_to_jira_users:
  john.doe: "557058:12345678-1234-1234-1234-123456789012"
  
cyberark_known_users:
  - john.doe
```

**Complete `config.yaml` example:**
```yaml
# GitHub to Jira Configuration File
# This file contains all non-secret configuration for the gh-to-jira bot

# =============================================================================
# GitHub Configuration
# =============================================================================
github_owner: "cert-manager"         # GitHub organization/user name
# github_repo: ""                    # Single repository (leave empty to use github_repos or scan all)
github_repos: []                     # List of specific repositories to monitor
github_label: "cybr"                 # GitHub label to filter issues/PRs

# =============================================================================
# Jira Project Configuration  
# =============================================================================
jira_project_key: "PROJ"             # REQUIRED: Jira project key
jira_issue_type: "Task"              # Jira issue type for created issues
jira_skip_description: true          # Set to false to include a GitHub link in the description field (default: true)

# =============================================================================
# Jira Status Mapping
# =============================================================================
jira_status_open: "To Do"            # Status for open GitHub issues/PRs
jira_status_closed: "Done"           # Status for closed/merged GitHub issues/PRs  
jira_status_draft: "In Progress"     # Status for draft PRs
jira_status_reopened: "Reopened"     # Status for reopening closed tickets
jira_resolution: "Done"              # Resolution when closing tickets

# =============================================================================
# User Mappings
# =============================================================================
github_to_jira_users:
  john.doe: "557058:12345678-1234-1234-1234-123456789012"
  jane.smith: "557058:87654321-4321-4321-4321-210987654321"

cyberark_known_users:
  - john.doe
  - jane.smith
```

To find Jira account IDs:
1. Go to your Jira instance
2. Navigate to People → View all people
3. Click on a user's profile
4. The account ID is in the URL: `https://yoursite.atlassian.net/jira/people/ACCOUNT_ID_HERE`

**Note on Jira Resolution:** When closing tickets, Jira often requires a resolution to be set. Common resolution values include:
- `"Done"` - Generic completion (default)
- `"Fixed"` - For bugs or issues that were resolved
- `"Resolved"` - General resolution
- `"Won't Do"` - For tickets that won't be addressed
- `"Duplicate"` - For duplicate tickets

Check your Jira project settings to see which resolutions are available.

Run
```
go run .
```

**Dry-run mode:** To see what actions would be taken without making changes:
```
go run . --dry-run
```

The bot runs continuously, polling GitHub every minute. Stop it with `Ctrl+C`.

What it does
- Lists all GitHub issues and PRs (state=all) in the selected repositories filtered by the configured label.
- For each item:
  - Checks Jira for an existing issue whose *environment* field contains `<owner>/<repo>#<n>` (uses GET `/rest/api/3/search/jql`, falls back to POST batch).
  - If not found, creates a Jira issue:
    - Summary: `<repo>#<n>: <issue title>` (you can edit it later, the prefix helps identify the source)
    - Labels: `github`, `cert-manager`, `gh-to-jira`, `<label>`, `repo:<repo>`
    - Environment: `Ref: <owner>/<repo>#<n> (do not edit this)` – The ticket's title and description can be edited freely after creation.
    - **Assignee**: Automatically determined based on GitHub issue/PR data (see Assignee Logic below).
  - Adds a Jira remote link back to the GitHub issue.
  - If the Jira ticket already exists, updates the label set, assignee, and resets the environment field while leaving the summary/description untouched so you can edit them.
  - **Status Mapping**: Automatically transitions Jira tickets based on GitHub state changes:
    - GitHub issues: `open` → any status except `JIRA_STATUS_CLOSED` (respects manual status changes), `closed` → `JIRA_STATUS_CLOSED` (default: "Done")
    - GitHub PRs: `open` → any status except `JIRA_STATUS_CLOSED` (respects manual status changes), `draft` → `JIRA_STATUS_DRAFT` (default: "In Progress"), `closed` or `merged` → `JIRA_STATUS_CLOSED`

Assignee Logic
The bot determines the Jira assignee based on GitHub issue/PR information with the following priority:
1. **Author first (if known at CyberArk)**: If the GitHub issue/PR author is listed in the `cyberark_known_users` section of the YAML config, they get priority for assignment.
2. **Assignees (if known at CyberArk)**: GitHub assignees who are known CyberArk users.
3. **Reviewers for PRs (if known at CyberArk)**: For PRs, requested reviewers who are known CyberArk users.
4. **Fallback to any assignee**: If no CyberArk users are found, falls back to any GitHub assignee.
5. **Fallback to any reviewer**: For PRs, falls back to any requested reviewer.

The GitHub username must be mapped to a Jira account ID in the `github_to_jira_users` section of the YAML config for the assignment to work.

Notes
- The description field is omitted by default, allowing you to add your own description. Set `jira_skip_description: false` to include a link back to the GitHub item in the description.
- If your Jira project requires additional fields, the bot fetches CreateMeta and auto-fills minimal valid values when creating.
- Duplicate detection relies on the environment field containing `<owner>/<repo>#<n>`. The bot rewrites that field each cycle, so leave the “(do not edit this)” marker in place.
- Jira issue properties could theoretically hold the GitHub identifiers, but making them searchable requires an administrator to configure entity-property indexing. Using the environment field avoids that administrative step.
- Summaries and descriptions are only set on initial creation; subsequent syncs leave them untouched so you can edit them freely.
- **Status transitions**: The bot only performs status transitions that are available in your Jira workflow. If a transition isn't available, it logs a warning and continues without error. Make sure your Jira workflow allows the transitions you need (e.g., "To Do" ↔ "Done", "In Progress" ↔ "Done").

Mapping details
- **Summary**: `<repo>#<number>: <GitHub title>` (truncated to fit Jira’s limits). Feel free to tweak the descriptive text after the prefix.
- **Labels**: Always include `github`, `cert-manager`, `gh-to-jira`, the configured GitHub label, and `repo:<repo>`, merged with any existing Jira labels.
- **Environment**: `Ref: <owner>/<repo>#<number> (do not edit this)` rendered as clickable link back to GitHub – this is how the bot finds the ticket on subsequent runs. The ticket's title and description can be edited freely after creation.
- **Assignee**: Automatically determined from GitHub issue/PR author, assignees, and reviewers (for PRs), prioritizing CyberArk known users. Requires mapping GitHub usernames to Jira account IDs.
