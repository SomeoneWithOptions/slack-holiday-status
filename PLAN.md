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
       Date      time.Time `json:"date"`
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

## 6. Slack app setup (one-time, manual)

1. Go to <https://api.slack.com/apps> → **Create New App** → *From scratch* →
   name "Colombia Holiday Status", pick your workspace.
2. **OAuth & Permissions** → **User Token Scopes** → add:
   - `users.profile:write`
   - `dnd:write`
3. **Install to Workspace** → approve → copy the **User OAuth Token**
   (starts with `xoxp-…`). This is your `SLACK_USER_TOKEN`.

---

## 7. GCP deployment commands (go in README)

```bash
# ---- variables ----
PROJECT=your-gcp-project
REGION=us-central1
REPO=slack-holiday-status
IMAGE=$REGION-docker.pkg.dev/$PROJECT/$REPO/app:latest

# ---- 0. enable APIs ----
gcloud config set project $PROJECT
gcloud services enable \
  run.googleapis.com \
  cloudscheduler.googleapis.com \
  artifactregistry.googleapis.com \
  cloudbuild.googleapis.com

# ---- 1. create Artifact Registry repo (once) ----
gcloud artifacts repositories create $REPO \
  --repository-format=docker \
  --location=$REGION

# ---- 2. build & push image ----
gcloud builds submit --tag $IMAGE

# ---- 3. create / update the Cloud Run Job ----
gcloud run jobs create slack-holiday-status \
  --image=$IMAGE \
  --region=$REGION \
  --max-retries=3 \
  --task-timeout=60s \
  --set-env-vars=SLACK_USER_TOKEN=xoxp-YOUR-TOKEN-HERE
# (use `gcloud run jobs update` for subsequent deploys)

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

## 8. Local development

```bash
export SLACK_USER_TOKEN=xoxp-…
go run .
```

On a non-holiday it logs and exits 0. On a holiday it actually changes your
Slack status & DND — there is no dry-run flag (per decision #14).

---

## 9. What gets created

When you give the go-ahead, I will create:

1. `go.mod` — module `github.com/<you>/slack-holiday-status`, Go 1.24
2. `go.sum` — generated by `go mod tidy`
3. `main.go` — ~150 LOC, structured per §4
4. `Dockerfile` — exactly as in §5
5. `.dockerignore` — `.git`, `*.md`, `PLAN.md`, etc.
6. `.gitignore` — compiled binary, `.env`
7. `README.md` — Slack app setup (§6) + full gcloud commands (§7) + local-run (§8)

Total: ~250 lines including README. No CI, no Terraform, no tests.
