Code comments must end with a dot like any sentence would. When using JSON, use encoding/json/v2, not encoding/json; don't forget to do 'export GOEXPERIMENT=jsonv2' when running Go commands.

When commiting, both provide a title AND a body to the commit. The body must explain the context around the commit, and must be wrapped at 72 chars (except for code snippets and such). Ask me if you are unsure of what the context is.

If you need to inspect a particular Jira ticket, use the command:

source .envrc && curl -sS -u "$JIRA_EMAIL:$JIRA_API_TOKEN" \
  "$JIRA_BASE_URL/rest/api/3/issue/VC-45980?expand=names"

If you need to inspect a GitHub issue or PR, use:

gh api /repos/cert-manager/cert-manager/issues/8171

Don't forget to update README.md if necessary if you change something user-facing like some change of behavior in the bot.
