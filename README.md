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
github_repos: [] # Empty = scan all repos in org
github_label: "cybr"

# Jira Configuration
jira_project_key: "PROJ" # Required
jira_issue_type: "Task"
jira_skip_description: true # Set false to add GitHub link in description

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

| Field Name        | Overwritten by gh-to-jira                            |
| ----------------- | ---------------------------------------------------- |
| Assignee          | Yes (set to the GitHub issue or PR assignee)         |
| Sprint            | No, just pre-filled with "cert-manager - OpenSource" |
| Fix Version       | No                                                   |
| Components        | Yes                                                  |
| Capacity Category | No, just pre-filled (see below)                      |
| Title             | No, just pre-filled                                  |
| Summary           | No                                                   |

### Capacity Category

The Capacity Category depends on GitHub issue or PR issue type, if the GitHub issue or PR has a type:

| GitHub Issue Type | Capacity Category in Jira |
| ----------------- | ------------------------- |
| Bug               | Maintenance               |
| Task              | Maintenance               |
| Feature           | Feature                   |
| (no type)         | Maintenance               |

### Assignee and Status

The Jira status depends on a few things:

| GitHub assignee   | GitHub status | Jira assignee     | Jira status                                                                                                           |
| ----------------- | ------------- | ----------------- | --------------------------------------------------------------------------------------------------------------------- |
| none              | Open          | none              | New                                                                                                                   |
| CyberArk employee | Open          | CyberArk employee | **In Progress** (since the bot guesses that if an assignee has been set, it means CyberArk employee is working on it) |
| none              | Closed        | none              | Closed                                                                                                                |

## Linking an existing Jira ticket

If you have an existing Jira ticket and you want to link it to a GitHub issue or
PR, add the following text to the Environment block:

```text
cert-manager/trust-manager#1234
```

Then, add the `cybr` label to the GitHub issue or PR. Wait for a bit and
gh-to-jira will link the existing Jira ticket to that GitHub issue or PR.

### Best Practice: Use a Parent GitHub Issue, Not Directly Linking PRs

In most cases, it's preferable to create a "parent" GitHub issue instead of
directly linking a GitHub PR to Jira. From the parent GitHub issue, you can link
to the various PRs. This approach provides better organization and tracking.

For example, see how this was done for
[cert-manager/cert-manager#8251](https://github.com/cert-manager/cert-manager/issues/8251),
where the issue serves as a central point linking to related PRs.

### Important: Un-linking is Not Possible

It's not possible to un-link a Jira ticket from a GitHub issue or PR once
linked. This is because Jira's search has some form of cache. Even after
removing the reference string (e.g., "Ref: cert-manager/cert-manager#1234") from
the Environment block, searching for that string in Jira will still work and the
link will persist.

## Can I change the Jira task to an epic?

Yes, and gh-to-jira will keep tracking it (the "Environment" field is carried
over, just hidden).
