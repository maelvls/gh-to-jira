gh-to-jira: Sync cert-manager issues (by milestone) to Jira

Overview
- A small Go bot that creates a Jira ticket for each GitHub issue in `cert-manager/cert-manager` that belongs to milestone `"1.19"`.
- Uses GitHub REST API and Jira Cloud REST API v3 (ADF description, remote link back to GitHub).
- Skips pull requests and avoids duplicates by searching Jira for a summary token like `cert-manager#<issue-number>`.

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
- `GITHUB_MILESTONE` default: `1.19` (title of the milestone)
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

What it does
- Resolves the configured milestone title to its number, and lists all GitHub issues (state=all) in that milestone.
- For each issue (skipping PRs):
  - Checks Jira for an existing issue with summary containing `cert-manager#<n>` (uses POST `/rest/api/3/search/jql`, with legacy fallback).
  - If not found, creates a Jira issue:
    - Summary: `cert-manager#<n>: <issue title>`
    - Labels: `github`, `cert-manager`, `milestone:<title>`
    - Description: ADF document linking to the GitHub issue and including metadata and body (omit with `JIRA_SKIP_DESCRIPTION=true`).
  - Adds a Jira remote link back to the GitHub issue.

Notes
- The description uses Atlassian Document Format (ADF); formatting is best-effort.
- If your Jira instance enforces custom required fields, set up appropriate field defaults or extend the code to populate them.
- The duplicate check is heuristic (by summary). If you already created issues with a different pattern, adjust `jiraFindExisting`.
