gh-to-jira: Sync labelled cert-manager issues/PRs to Jira

Overview
- A small Go bot that creates/updates Jira tickets for GitHub issues and PRs in `cert-manager/cert-manager` carrying a specific label (default `cybr`), polling every minute.
- Uses GitHub REST API and Jira Cloud REST API v3 (ADF description, remote link back to GitHub).
- Avoids duplicates by searching Jira for a summary token like `cert-manager#<issue-number>`.

Prerequisites
- Go 1.22+
- A GitHub token with `repo:read` scope in `GITHUB_TOKEN`.
- A Jira Cloud site and a project key. Create a Jira API token and set the following env vars:
  - `JIRA_BASE_URL` (e.g., `https://your-domain.atlassian.net`)
  - `JIRA_EMAIL` (your Atlassian account email)
  - `JIRA_API_TOKEN` (API token)
  - `JIRA_PROJECT_KEY` (e.g., `ABC`)

Environment Variables
- `GITHUB_TOKEN` (required)
- `GITHUB_OWNER` default: `cert-manager`
- `GITHUB_REPO` default: `cert-manager`
- `GITHUB_LABEL` default: `cybr`
- `JIRA_BASE_URL`, `JIRA_EMAIL`, `JIRA_API_TOKEN`, `JIRA_PROJECT_KEY` (all required)
- `JIRA_PROJECT_ID` optional; if set, used instead of `JIRA_PROJECT_KEY` when creating issues
- `JIRA_ISSUE_TYPE` default: `Task` (name)
- `JIRA_ISSUE_TYPE_ID` optional; if set, used instead of `JIRA_ISSUE_TYPE` when creating issues
- `DRY_RUN` set to `true` to only print actions
- `JIRA_SKIP_DESCRIPTION` set to `true` to omit the Jira description field (useful if your project requires fields or rejects ADF and returns 400)

Run
```
go run .
```

The bot runs continuously, polling GitHub every minute. Stop it with `Ctrl+C`.

- Lists all GitHub issues and PRs (state=all) filtered by the configured label.
- For each item:
  - Checks Jira for an existing issue with summary containing `cert-manager#<n>` (uses GET `/rest/api/3/search/jql`, falls back to POST batch).
  - If not found, creates a Jira issue:
    - Summary: `cert-manager#<n>: <issue title>`
    - Labels: `github`, `cert-manager`, `<label>`
    - Description: ADF with a link to the GitHub item and a truncated copy of the body (omit with `JIRA_SKIP_DESCRIPTION=true`).
  - Adds a Jira remote link back to the GitHub issue.
  - If the Jira ticket already exists, updates summary/labels/description.

Notes
- The description uses Atlassian Document Format (ADF) with truncation to avoid oversized payloads.
- If your Jira project requires additional fields, the bot fetches CreateMeta and auto-fills minimal valid values when creating.
- Duplicate detection is based on summary token `cert-manager#<n>`. If you use a different convention, adjust `jiraFindExisting`.
