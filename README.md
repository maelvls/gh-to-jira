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

# Jira Team Field (optional)
jira_team_field_key: "customfield_10211"
jira_team_option_id: "13667"

# Jira Components
jira_default_component: "cert-manager"
jira_components:
  cert-manager: "cert-manager"
  approver-policy: "Approver Policy (OSS)"
  trust-manager: "Trust Manager (OSS)"

# Status Mapping
jira_status_open: "To Do"
jira_status_closed: "Done"
jira_status_in_progress: "In Progress"
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
- Auto-assigns from GitHub assignees unless the reporter is a CyberArk user.
- Syncs status: open without assignee → non-closed, assigned items → promoted to In Progress when Jira is New/Closed, closed/merged → closed, draft → In Progress.
- Updates labels/assignee each sync; leaves summary/description editable

**Labels applied to Jira** `OpenSource`, `gh-to-jira`, `repo:<repo-name>`

## What fields are managed by gh-to-jira?

gh-to-jira overwrites some fields in the Jira ticket when syncing from GitHub:

|    Field Name     |              Overwritten by gh-to-jira               |
|-------------------|------------------------------------------------------|
| Assignee          | Yes (set to the GitHub issue or PR assignee)         |
| Sprint            | No, just pre-filled with "cert-manager - OpenSource" |
| Fix Version       | No                                                   |
| Components        | Yes                                                  |
| Capacity Category | No, just pre-filled \*                               |
| Title             | No, just pre-filled                                  |
| Summary           | No                                                   |

\* The "Capacity Category" field in the Jira ticket is set depending on the
GitHub issue or PR issue type:

| GitHub Issue Type | Capacity Category in Jira |
|-------------------|---------------------------|
| Bug               | Maintenance               |
| Task              | Maintenance               |
| Feature           | Feature                   |

## Notes

- Don't edit the environment field marker `(do not edit this)` - used for duplicate detection
- Status transitions require proper Jira workflow configuration
- Bot auto-fills required Jira fields using CreateMeta
