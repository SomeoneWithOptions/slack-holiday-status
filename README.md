# slack-holiday-status

Automates Slack holiday status for Colombian public holidays.

## Local configuration

The Slack user token is read from `.env`:

```bash
SLACK_USER_TOKEN=xoxp-your-user-token-here
```

`.env` is intentionally ignored by git.

To load it locally when testing scripts/commands:

```bash
set -a
. ./.env
set +a
```

The token must be a Slack **user token** for the target user, with scopes:

- `users.profile:write`
- `dnd:write`

For the current setup, the token in `.env` has been verified with Slack `auth.test` as user `acastellanos`.
