# gh-to-jira

Syncs labeled GitHub issues/PRs to Jira tickets. Polls every minute, creates or updates Jira tickets for issues/PRs with a specific label.

## Quick Start

**Required environment variables:**
```bash
export GITHUB_TOKEN="ghp_..."
export JIRA_BASE_URL="https://your-domain.atlassian.net"
export JIRA_EMAIL="you@example.com"
export JIRA_API_TOKEN="..."
```

**Minimal `config.yaml`:**
```yaml
jira_project_key: "PROJ"
github_owner: "cert-manager"
github_label: "cybr"

github_to_jira_users:
  githubuser: "557058:12345678-1234-1234-1234-123456789012"

cyberark_known_users:
  - githubuser
```

**Run:**
```bash
go run .              # Live mode
go run . --dry-run    # Preview mode
```

## Configuration

All non-secret config goes in `config.yaml`. Defaults work for most settings.

<details>
<summary>Complete config.yaml example</summary>

```yaml
# GitHub Configuration
github_owner: "cert-manager"
github_repos: []              # Empty = scan all repos in org
github_label: "cybr"

# Jira Configuration
jira_project_key: "PROJ"      # Required
jira_issue_type: "Task"
jira_skip_description: true   # Set false to add GitHub link in description

# Status Mapping
jira_status_open: "To Do"
jira_status_closed: "Done"
jira_status_draft: "In Progress"
jira_status_reopened: "Reopened"
jira_resolution: "Done"

# User Mappings
github_to_jira_users:
  john.doe: "557058:12345678-1234-1234-1234-123456789012"

cyberark_known_users:
  - john.doe
```
</details>

**Find Jira account IDs:** People → View all people → Click user → Copy ID from URL.

## How It Works

- Scans repos for issues/PRs with the configured label
- Creates Jira tickets: `<repo>#<n>: <title>`
- Uses environment field for duplicate detection (`<owner>/<repo>#<n>`)
- Auto-assigns based on GitHub author/assignees/reviewers (prioritizes known users)
- Syncs status: open → non-closed, closed/merged → closed, draft → in-progress
- Updates labels/assignee each sync; leaves summary/description editable

## Notes

- Don't edit the environment field marker `(do not edit this)` - used for duplicate detection
- Status transitions require proper Jira workflow configuration
- Bot auto-fills required Jira fields using CreateMeta
