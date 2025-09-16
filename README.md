gh-to-jira: Sync labelled cert-manager issues/PRs to Jira

Overview
- A small Go bot that creates/updates Jira tickets for GitHub issues and PRs across the `cert-manager` organisation carrying a specific label (default `cybr`), polling every minute.
- Uses GitHub REST API and Jira Cloud REST API v3 (ADF description, remote link back to GitHub).
- Avoids duplicates by storing a Jira issue property that records the GitHub owner, repo, and issue/PR number.

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
- `GITHUB_LABEL` default: `cybr`
- `GITHUB_REPO` optional; limit processing to a single repository
- `GITHUB_REPOS` optional; comma-separated list of repositories to process (takes precedence over `GITHUB_REPO`)
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

What it does
- Lists all GitHub issues and PRs (state=all) in the selected repositories filtered by the configured label.
- For each item:
  - Checks Jira for an existing issue whose *environment* field contains `<owner>/<repo>#<n>` (uses GET `/rest/api/3/search/jql`, falls back to POST batch).
  - If not found, creates a Jira issue:
    - Summary: `<repo>#<n>: <issue title>` (you can edit it later, the prefix helps identify the source)
    - Labels: `github`, `cert-manager`, `<label>`, `repo:<repo>`
    - Environment: `Ref: <owner>/<repo>#<n> (do not edit this)`
    - Description: ADF document containing only a link back to the GitHub item (omit entirely with `JIRA_SKIP_DESCRIPTION=true`).
  - Adds a Jira remote link back to the GitHub issue.
  - If the Jira ticket already exists, updates the label set and resets the environment field while leaving the summary/description untouched so you can edit them.

Notes
- The description uses Atlassian Document Format (ADF) and now only contains a single link back to the GitHub item.
- If your Jira project requires additional fields, the bot fetches CreateMeta and auto-fills minimal valid values when creating.
- Duplicate detection relies on the environment field containing `<owner>/<repo>#<n>`. The bot rewrites that field each cycle, so leave the “(do not edit this)” marker in place.
- Jira issue properties could theoretically hold the GitHub identifiers, but making them searchable requires an administrator to configure entity-property indexing. Using the environment field avoids that administrative step.
- Summaries and descriptions are only set on initial creation; subsequent syncs leave them untouched so you can edit them freely.

Mapping details
- **Summary**: `<repo>#<number>: <GitHub title>` (truncated to fit Jira’s limits). Feel free to tweak the descriptive text after the prefix.
- **Labels**: Always include `github`, `cert-manager`, the configured GitHub label, and `repo:<repo>`, merged with any existing Jira labels.
- **Environment**: `Ref: <owner>/<repo>#<number> (do not edit this)` rendered as clickable link back to GitHub – this is how the bot finds the ticket on subsequent runs.
- **Description**: Single paragraph with a link labelled `GitHub <owner>/<repo>#<number>` pointing to the issue/PR; no body content is copied.
