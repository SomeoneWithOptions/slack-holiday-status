# Detailed Plan — `slack-holiday-status`

A Cloud Run Job, written in idiomatic Go with minimal dependencies, that
checks each morning whether today is a public holiday in Colombia and, if so,
sets a Slack custom status and pauses notifications (DND) until end of day.

---

## 1. Feasibility ✅

| Capability | API / Mechanism | Notes |
|---|---|---|
| Check if today is a Colombian holiday | `GET https://api.diafestivo.co/next` → `{name, date, isToday, daysUntil}` | Public, no auth. We use `isToday` to decide, `name` for the status text. |
| Set Slack custom status | `POST https://slack.com/api/users.profile.set` with `status_text`, `status_emoji`, `status_expiration` | Requires **user token** (`xoxp-`) with scope `users.profile:write`. |
| Pause Slack notifications | `POST https://slack.com/api/dnd.setSnooze` with `num_minutes` | Requires **user token** (`xoxp-`) with scope `dnd:write`. |
| Daily trigger | Cloud Run **Job** + Cloud Scheduler → Jobs API (`*:run`) with OIDC | Canonical pattern. No HTTP server in our code. |

Bot tokens (`xoxb-`) cannot set user status or DND — a user token from a
Slack app installed to your own workspace is required.

---

## 2. Locked-in design decisions

| # | Decision |
|---|----------|
| 1  | Cloud Run **Job** (not Service) triggered by Cloud Scheduler via Jobs API |
| 2  | New Slack app, user token passed as plain `--set-env-vars SLACK_USER_TOKEN=…` |
| 3  | Status: emoji `:palm_tree:`, text `Colombia Holiday: {name}` |
| 4  | `status_expiration` = end-of-day in `America/Bogota` (23:59:59 local) |
| 5  | DND `num_minutes` = minutes from now until end-of-day Bogota |
| 6  | Schedule: `0 6 * * *` in `America/Bogota`, daily Mon–Sun |
| 7  | Non-holiday → exit 0 silently, touch nothing in Slack |
| 8  | Fail-fast on errors, exit non-zero; Cloud Run Job `--max-retries=3`; per-request 10s HTTP timeout |
| 9  | Deps: stdlib + `github.com/slack-go/slack` |
| 10 | Go 1.24, multi-stage Dockerfile → `gcr.io/distroless/static-debian12:nonroot` |
| 11 | Dockerfile + README with `gcloud` commands (no Terraform / Cloud Build) |
| 12 | No tests (tiny script, manual verification via `gcloud run jobs execute`) |
| 13 | Structured JSON logs via `log/slog` (stdlib) |
| 14 | Only `SLACK_USER_TOKEN` is configurable via env; everything else is a `const` |

---

## 3. Repository layout

```
slack-holiday-status/
├── main.go              # entire program, ~150 LOC
├── go.mod
├── go.sum
├── Dockerfile           # multi-stage, distroless final image
├── .dockerignore
├── .gitignore
├── README.md            # Slack app setup + gcloud deploy commands + local run
├── PLAN.md              # this file
└── requirements.txt     # original requirements (kept for reference)
```

---

## 4. Program design (`main.go`)

### Constants

```go
const (
    diafestivoURL = "https://api.diafestivo.co/next"
    statusEmoji   = ":palm_tree:"
    statusTextFmt = "Colombia Holiday: %s"
    tzName        = "America/Bogota"
    httpTimeout   = 10 * time.Second
)
```

### Control flow — `run() error`

`main` only does: configure slog → call `run()` → on error, `slog.Error` and
`os.Exit(1)`. All real logic lives in `run()`.

1. **Load timezone** `America/Bogota` via `time.LoadLocation`. Fail if missing
   (sanity check — distroless static ships zoneinfo).
2. **Read** `SLACK_USER_TOKEN` env var; fail if empty.
3. **Compute** `endOfDay := time.Date(y, m, d, 23, 59, 59, 0, loc)` using
   `time.Now().In(loc)`.
4. **Fetch `/next`** with an `http.Client{Timeout: httpTimeout}`. Decode into:
   ```go
   type nextHoliday struct {
       Name      string    `json:"name"`
       Date      string    `json:"date"`
       IsToday   bool      `json:"isToday"`
       DaysUntil int       `json:"daysUntil"`
   }
   ```
   If `!IsToday`: `slog.Info("not a holiday today", "next", h.Name, "days_until", h.DaysUntil)` → return nil (exit 0).
5. **Build Slack client**: `slack.New(token, slack.OptionHTTPClient(&http.Client{Timeout: httpTimeout}))`.
6. **Set status**:
   `api.SetUserCustomStatus(fmt.Sprintf(statusTextFmt, h.Name), statusEmoji, endOfDay.Unix())`.
7. **Set DND**:
   `minutes := int(time.Until(endOfDay).Minutes()) + 1` (round up so we never undershoot midnight)
   `api.SetSnooze(minutes)`.
8. **Log success**: `slog.Info("holiday status applied", "holiday", h.Name, "expires_at", endOfDay, "dnd_minutes", minutes)`.

Any non-nil error from steps 1–7 is wrapped with `fmt.Errorf("…: %w", err)`
and returned. Cloud Run Job sees non-zero exit and retries up to 3 times.

### Logging setup

```go
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
```

Cloud Logging auto-parses JSON, giving queryable fields (`holiday`,
`expires_at`, etc.).

---

## 5. `Dockerfile`

```dockerfile
# ---- build stage ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/app .

# ---- runtime stage ----
# distroless/static includes /usr/share/zoneinfo, CA certs, and a nonroot user
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
```

Resulting image: ~10 MB, no shell, static binary, runs as non-root.

---

## 6. Slack app setup — generating `SLACK_USER_TOKEN` (one-time, ~5 min)

The job requires a **user token** (`xoxp-…`), *not* a bot token. Bot tokens
(`xoxb-`) have no permission to set your profile or DND.

---

### Phase A — Create the Slack app

1. Open <https://api.slack.com/apps> in a browser and click
   **Create New App → From scratch**.
2. **App name**: `Colombia Holiday Status`
3. **Pick a workspace**: choose the workspace whose status you want to
   manage. Click **Create App**.
4. You land on the app's **Basic Information** page.

---

### Phase B — Grant OAuth scopes

1. In the left sidebar, click **OAuth & Permissions**.
2. Scroll to **User Token Scopes** and click **Add an OAuth Scope**.
3. Add these two scopes:

   | Scope | What it allows |
   |---|---|
   | `users.profile:write` | Set / clear your custom status |
   | `dnd:write` | Enable / disable DND (Do Not Disturb) |

4. Do **not** add scopes under **Bot Token Scopes** — the job never uses
   a bot token.

---

### Phase C — Install to your workspace & get the token

1. Near the top of the **OAuth & Permissions** page, click the blue
   **Install to Workspace** button (you may need to scroll up to see it,
   it sits below the OAuth scopes block).
2. Slack will show a permission summary screen listing the two user
   scopes you added. Click **Allow**.
3. On the next screen, **Your New Token** is shown at the top under
   **Your New User Token**. It looks like:

   ```text
   xoxp-1234567890123-1234567890123-1234567890123-somehex
   ```

4. Click **Copy** to copy the token to your clipboard.
5. **Paste it into the `SLACK_USER_TOKEN` environment variable** when you
   create the Cloud Run Job (step 3 of the deployment commands in §8).

> "Slack is token": Copy this once, treat it like a password — anyone
> with it can change your status, DND, and more. Never commit it to git
> or share it in Slack UI.

---

### Phase D — Verify the token works

Before deploying, it is worth a quick test:

```bash
export SLACK_USER_TOKEN=xoxp-your-pasted-token
# sets a temporary status just for you — visible to everyone you share
# a channel with, lasts 30 min by default
curl -X POST https://slack.com/api/users.profile.set \
  -H "Authorization: Bearer $SLACK_USER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile":{"status_text":"testing token","status_emoji":":test_tube:","status_expiration":0}}'
```

Pick up the API response (if the error message doesn't make sense,
see <https://api.slack.com/methods/users.profile.set>).

---

## 7. Local build & push (`build-and-push.sh`) — run after every code change

Rather than paying for Cloud Build to compile a ~10 MB Go binary, build
locally with Docker and push the image directly to Artifact Registry. The script
then updates the Cloud Run Job to point at the newly pushed image.

### Prerequisites (one-time on your machine)

- **Docker Desktop** (or `colima` / `podman`) with `buildx` — required to build/push a `linux/amd64` image reliably from any laptop architecture.
- **gcloud authenticating Docker** for Artifact Registry push:
  ```bash
  gcloud auth configure-docker us-central1-docker.pkg.dev
  ```
  This drops a `~/.docker/config.json` credential helper so `docker push`
  authenticates without any flags.
- **Go 1.24+** SDK installed locally (`go version` ≥ 1.24).

---

### The script

Save this as `build-and-push.sh` in the repo root:

```bash
#!/usr/bin/env bash
# build-and-push.sh
# Build locally, push the image to Artifact Registry, and update the Cloud Run Job.
# Usage:  ./build-and-push.sh [--tag v1.2.3]  (defaults to :latest)

set -euo pipefail

# ---- 1. defaults & flags ----
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGION="us-central1"
ACCOUNT="acastellanos@recurly.com"
PROJECT="it-tools-4fc1b6dd"
REPO="slack-holiday-status"
REGISTRY_HOST="${REGION}-docker.pkg.dev"
IMAGE_REGISTRY="${REGISTRY_HOST}/${PROJECT}/${REPO}"
TAG="latest"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)  TAG="$2"; shift 2 ;;
    *)      echo "Unknown arg: $1"; exit 1 ;;
  esac
done

IMAGE="${IMAGE_REGISTRY}/app:${TAG}"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo " Account  : $ACCOUNT"
echo " Project  : $PROJECT"
echo " Image    : $IMAGE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# ---- 2. pre-flight checks ----
for cmd in docker go gcloud; do
  command -v "$cmd" > /dev/null 2>&1 || { echo "❌ '$cmd' is not installed or not in PATH"; exit 1; }
done

echo "✓ Pre-flight checks passed"

# ---- 3. ensure gcloud → Artifact Registry credential helper is set ----
if ! grep -q "${REGISTRY_HOST}" ~/.docker/config.json 2>/dev/null; then
  echo "→ One-time: running  gcloud auth configure-docker …"
  gcloud auth configure-docker "${REGISTRY_HOST}" --quiet
fi

echo "✓ Docker → Artifact Registry auth configured"

# ---- 4. gcloud project guard ----
echo "→ Setting gcloud project → $PROJECT"
gcloud config set project "$PROJECT" > /dev/null

echo "✓ Project set"

# ---- 5. Go module download (cached between runs) ----
echo "→ Downloading Go module dependencies …"
cd "$SCRIPT_DIR"
go mod download

echo "✓ Dependencies downloaded"

# ---- 6. build multi-stage image & push ----
docker buildx build \
  --platform linux/amd64 \
  -f "$SCRIPT_DIR/Dockerfile" \
  -t "$IMAGE" \
  --push \
  "$SCRIPT_DIR"

echo "✓ Image built and pushed: $IMAGE"

# ---- 7. update Cloud Run Job to point at this image, if it already exists ----
if gcloud run jobs describe slack-holiday-status --region="$REGION" > /dev/null 2>&1; then
  gcloud run jobs update slack-holiday-status \
    --image="$IMAGE" \
    --region="$REGION"
  echo "✓ Cloud Run Job updated: slack-holiday-status → $IMAGE"
else
  echo "ℹ Cloud Run Job does not exist yet; create it once with the image above."
fi

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  ✅  $IMAGE"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
```

Make it executable:

```bash
chmod +x build-and-push.sh
```

---

### When to run it

- **First deploy** — run after you push the initial code to this repo and
  `go mod tidy` succeeds.
- **Every code change** — touch the job or constants, then re-run the script
  before updating the Cloud Run Job.
- **At release time** — use `--tag v0.1.0` to version distinct releases;
  keep `:latest` pointing at the current stable build.

---

### Cost note

Your laptop does the CPU-heavy Go compilation. GCP only ingests the final
~10 MB layer. This replaces `gcloud builds submit` entirely and comes at **zero
compute cost** on Google Cloud — only egress disk charges (~pennies/month at
this scale) apply.

---

## 8. GCP deployment commands (go in README)

```bash
# ---- variables ----
ACCOUNT=acastellanos@recurly.com
PROJECT=it-tools-4fc1b6dd
REGION=us-central1
REPO=slack-holiday-status
IMAGE=$REGION-docker.pkg.dev/$PROJECT/$REPO/app:latest

# ---- 0. enable APIs ----
gcloud auth login $ACCOUNT
gcloud config set project $PROJECT

gcloud services enable \
  run.googleapis.com \
  cloudscheduler.googleapis.com \
  artifactregistry.googleapis.com

# (cloudbuild.googleapis.com NOT enabled — we build locally with build-and-push.sh,
#  saving Google compute charges)

# ---- 1. create Artifact Registry repo (once) ----
gcloud artifacts repositories create $REPO \
  --repository-format=docker \
  --location=$REGION

# ---- 1b. first-time Docker → Artifact Registry credential helper ----
gcloud auth configure-docker ${REGION}-docker.pkg.dev

# ---- 2. build the image locally and push it to Artifact Registry ----
#     No Cloud Build charges — your laptop does the compilation.
#     If the job already exists, this script also updates it to the pushed image.
./build-and-push.sh
# ./build-and-push.sh --tag v0.1.0   # version a release

# ---- 3. create the Cloud Run Job on first deploy ----
gcloud run jobs create slack-holiday-status \
  --image=$IMAGE \
  --region=$REGION \
  --max-retries=3 \
  --task-timeout=60s \
  --set-env-vars=SLACK_USER_TOKEN=xoxp-YOUR-TOKEN-HERE
# For subsequent deploys, run `./build-and-push.sh --tag v0.1.1`; it pushes the
# new image and runs `gcloud run jobs update --image=...` automatically.

# ---- 4. service account for Scheduler → Job invocation ----
SA=scheduler-invoker@$PROJECT.iam.gserviceaccount.com
gcloud iam service-accounts create scheduler-invoker
gcloud run jobs add-iam-policy-binding slack-holiday-status \
  --region=$REGION \
  --member=serviceAccount:$SA \
  --role=roles/run.invoker

# ---- 5. Cloud Scheduler — 06:00 Bogota daily ----
gcloud scheduler jobs create http slack-holiday-status-daily \
  --location=$REGION \
  --schedule="0 6 * * *" \
  --time-zone="America/Bogota" \
  --uri="https://$REGION-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/$PROJECT/jobs/slack-holiday-status:run" \
  --http-method=POST \
  --oauth-service-account-email=$SA
```

Manual one-off execution (for testing):

```bash
gcloud run jobs execute slack-holiday-status --region=$REGION --wait
```

---

## 9. Local development

```bash
export SLACK_USER_TOKEN=xoxp-…
go run .
```

On a non-holiday it logs and exits 0. On a holiday it actually changes your
Slack status & DND — there is no dry-run flag (per decision #14).

---

## 10. What gets created

When you give the go-ahead, I will create:

1. `go.mod` — module `github.com/<you>/slack-holiday-status`, Go 1.24
2. `go.sum` — generated by `go mod tidy`
3. `main.go` — ~150 LOC, structured per §4
4. `Dockerfile` — exactly as in §5
5. `.dockerignore` — `.git`, `*.md`, `PLAN.md`, etc.
6. `.gitignore` — compiled binary, `.env`
7. `build-and-push.sh` — local Docker build & Artifact Registry push script (per §7)
8. `README.md` — Slack app setup (§6) + build script (§7) + gcloud commands (§8) + local-run (§9)

Total: ~280 lines including README + build script. No CI, no Terraform, no tests.
