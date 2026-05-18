# slack-holiday-status

A Cloud Run Job written in Go that checks every morning whether today is a
public holiday in Colombia and, if so, sets a Slack custom status and pauses
notifications (DND) until end of day.

---

## How it works

1. Calls [`api.diafestivo.co/next`](https://api.diafestivo.co) — a public API
   that returns the next Colombian public holiday and whether it falls today.
2. If **not** a holiday: logs and exits 0, touching nothing in Slack.
3. If **today is a holiday**:
   - Sets your Slack custom status to `🇨🇴 Colombia Holiday: {name}`, expiring
     at 23:59:59 `America/Bogota`.
   - Enables DND (snooze) for the remaining minutes of the day.
4. Structured JSON logs are emitted to stdout and collected by Cloud Logging.

All behavior is driven by a single env var (`SLACK_USER_TOKEN`). Every other
parameter is a constant in `main.go`.

---

## Repository layout

```
slack-holiday-status/
├── main.go              # entire program (~90 LOC)
├── go.mod
├── go.sum
├── Dockerfile           # multi-stage: golang:1.26.3-alpine → distroless/static:nonroot
├── .dockerignore
├── .gitignore
├── build-and-push.sh    # local Docker build + Artifact Registry push + job update
├── README.md
├── PLAN.md
└── requirements.txt     # original plain-English requirements (kept for reference)
```

---

## Infrastructure deployed

| Resource | Name | Details |
|---|---|---|
| **Artifact Registry repo** | `slack-holiday-status` | `us-central1`, Docker format |
| **Container image** | `app:latest` | `linux/amd64`, ~10 MB distroless image |
| **Cloud Run Job** | `slack-holiday-status` | `us-central1`, max-retries=3, 60 s timeout |
| **IAM Service Account** | `scheduler-invoker` | Used by Cloud Scheduler to invoke the job |
| **Cloud Scheduler job** | `slack-holiday-status-daily` | `0 2 * * *` `America/Bogota` — runs every day at 2 AM Bogota time |

The Scheduler authenticates to the Cloud Run Jobs API using OAuth with the
`scheduler-invoker` service account, which holds `roles/run.invoker` on the
job.

---

## Slack app setup (one-time, ~5 min)

A **user token** (`xoxp-…`) is required — bot tokens cannot set your profile
or DND on your behalf.

1. Go to <https://api.slack.com/apps> → **Create New App → From scratch**.
2. Name it (e.g. `Colombia Holiday Status`) and pick your workspace.
3. In the sidebar → **OAuth & Permissions** → **User Token Scopes**, add:
   - `users.profile:write`
   - `dnd:write`
4. Click **Install to Workspace** → **Allow**.
5. Copy the `xoxp-…` token that appears — treat it as a password, never commit
   it to git.

Verify the token works before deploying:

```bash
curl -X POST https://slack.com/api/auth.test \
  -H "Authorization: Bearer $SLACK_USER_TOKEN"
```

---

## Prerequisites (local machine)

- Go 1.26.3+
- Docker with `buildx` (Docker Desktop or OrbStack)
- `gcloud` CLI authenticated to your GCP project

One-time Docker credential setup for Artifact Registry:

```bash
gcloud auth configure-docker us-central1-docker.pkg.dev
```

---

## First-time GCP setup

```bash
PROJECT=your-gcp-project-id
REGION=us-central1

# Enable required APIs
gcloud services enable \
  run.googleapis.com \
  cloudscheduler.googleapis.com \
  artifactregistry.googleapis.com

# Create Artifact Registry repository
gcloud artifacts repositories create slack-holiday-status \
  --repository-format=docker \
  --location=$REGION

# Build image locally and push to Artifact Registry
./build-and-push.sh

# Create the Cloud Run Job
gcloud run jobs create slack-holiday-status \
  --image=us-central1-docker.pkg.dev/$PROJECT/slack-holiday-status/app:latest \
  --region=$REGION \
  --max-retries=3 \
  --task-timeout=60s \
  --set-env-vars=SLACK_USER_TOKEN=xoxp-your-token-here

# Create a service account for the scheduler
gcloud iam service-accounts create scheduler-invoker \
  --display-name="Cloud Scheduler — Cloud Run Job Invoker"

# Grant it permission to invoke the job
gcloud run jobs add-iam-policy-binding slack-holiday-status \
  --region=$REGION \
  --member="serviceAccount:scheduler-invoker@$PROJECT.iam.gserviceaccount.com" \
  --role=roles/run.invoker

# Get your project number (needed for the Jobs API URI)
PROJECT_NUMBER=$(gcloud projects describe $PROJECT --format="value(projectNumber)")

# Create the daily Cloud Scheduler trigger
gcloud scheduler jobs create http slack-holiday-status-daily \
  --location=$REGION \
  --schedule="0 2 * * *" \
  --time-zone="America/Bogota" \
  --uri="https://${REGION}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_NUMBER}/jobs/slack-holiday-status:run" \
  --http-method=POST \
  --oauth-service-account-email="scheduler-invoker@$PROJECT.iam.gserviceaccount.com"
```

---

## Redeploying after code changes

```bash
./build-and-push.sh
# Builds linux/amd64 image locally, pushes to Artifact Registry,
# and updates the Cloud Run Job to the new image automatically.

# To tag a specific release:
./build-and-push.sh --tag v0.2.0
```

No Cloud Build charges — your local machine does the compilation.

---

## Manual test run

```bash
gcloud run jobs execute slack-holiday-status --region=us-central1 --wait
```

Check the output:

```bash
gcloud logging read \
  'resource.type="cloud_run_job" AND resource.labels.job_name="slack-holiday-status"' \
  --limit=10 --format="value(jsonPayload)"
```

On a non-holiday you'll see:
```json
{"level":"INFO","msg":"not a holiday today","next":"...","days_until":3}
```

On a holiday:
```json
{"level":"INFO","msg":"holiday status applied","holiday":"la Ascensión del Señor","expires_at":"...","dnd_minutes":827}
```

---

## Local development

```bash
# Load token from .env (git-ignored)
set -a; . ./.env; set +a

go run .
```

On a holiday this will actually set your Slack status and DND — there is no
dry-run mode.

---

## Configuration reference

| Name | Where | Description |
|---|---|---|
| `SLACK_USER_TOKEN` | Cloud Run Job env var / `.env` | Slack user token (`xoxp-…`) |
| `statusEmoji` | `main.go` constant | Emoji shown in status (`:flag-co:`) |
| `statusTextFmt` | `main.go` constant | Status text template |
| `diafestivoURL` | `main.go` constant | Holiday API endpoint |
| `httpTimeout` | `main.go` constant | Per-request HTTP timeout (10 s) |
