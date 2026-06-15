# Lambda Migration & GitHub Actions Deployment Plan

This document captures the plan for migrating `slack-holiday-status` from a Cloud Run Job-oriented deployment to an AWS Lambda deployment, with GitHub Actions CI/CD using AWS OIDC authentication and no static AWS access keys.

No implementation has been done yet. This is the agreed plan for later execution.

---

## 1. Current repository/account state

### Git state

- Current branch: `lambda`
- Working tree: clean at time of planning
- Remote repo:

```text
git@github.com:SomeoneWithOptions/slack-holiday-status.git
```

### Current app shape

The app is currently a Go command-line program:

- `main.go` has `main()` calling `run()` directly.
- `run()`:
  - loads `America/Bogota` timezone
  - reads `SLACK_USER_TOKEN`
  - calls `https://api.diafestivo.co/next`
  - exits successfully if today is not a Colombian holiday
  - if holiday, checks Slack profile status
  - skips if status is already manually set
  - otherwise sets Slack custom status and DND until end of day
- Current deployment artifacts are GCP/Cloud Run oriented:
  - `Dockerfile`
  - `build-and-push.sh`
  - README and existing `PLAN.md` reference GCP Cloud Run Job / Cloud Scheduler

### Local tooling observed

```text
Go:      go1.26.4 darwin/arm64
Docker:  Docker version 29.4.0
AWS CLI: aws-cli/2.35.4
```

### AWS active account observed

Using local AWS CLI:

```text
Account ID: 745912973548
Account alias: sanetomore
Caller: arn:aws:iam::745912973548:user/andres
Default region: us-east-1
User group: Administrators
Group policy: AdministratorAccess
```

### Existing AWS resources checked

In `us-east-1`, the following were **not found** at time of planning:

- Lambda function `slack-holiday-status`
- ECR repository `slack-holiday-status`
- IAM role `slack-holiday-status-lambda-role`
- Secrets Manager secret `slack-holiday-status/slack-user-token`
- EventBridge rules or Scheduler schedules matching `slack` / `holiday`

Existing GitHub OIDC provider **does exist**:

```text
arn:aws:iam::745912973548:oidc-provider/token.actions.githubusercontent.com
```

Existing generic GitHub Actions role also exists:

```text
arn:aws:iam::745912973548:role/githubactions-role
```

But it currently trusts broad repo pattern:

```text
repo:SomeoneWithOptions/*:*
```

and only has ECR push permissions. For this project, create a new narrow role instead of reusing it.

---

## 2. Target architecture

### Runtime

Use AWS Lambda zip deployment with Go custom runtime:

```text
Runtime: provided.al2023
Architecture: arm64
Package type: Zip
Handler: bootstrap
```

Rationale:

- AWS `go1.x` managed Lambda runtime is deprecated.
- AWS recommends Go Lambda functions use OS-only runtimes like `provided.al2023`.
- Zip deploy is simpler than Lambda container image deploy for this small Go binary.
- Zip deploy avoids needing ECR for this project.

### AWS resources to create

| Resource | Name | Purpose |
|---|---|---|
| Lambda function | `slack-holiday-status` | Runs the holiday check and Slack update |
| Lambda execution role | `slack-holiday-status-lambda-role` | Allows Lambda to write logs |
| EventBridge Scheduler role | `slack-holiday-status-scheduler-role` | Allows Scheduler to invoke Lambda |
| EventBridge Scheduler schedule | `slack-holiday-status-daily` | Runs Lambda daily at 02:00 Bogota time |
| GitHub Actions deploy role | `githubactions-slack-holiday-status-lambda` | Allows GitHub Actions to deploy Lambda code via OIDC |

### Secret handling

Initial plan: store `SLACK_USER_TOKEN` as a Lambda environment variable.

Important:

- Do **not** put `SLACK_USER_TOKEN` in GitHub secrets unless later required.
- GitHub Actions should update Lambda code only via `update-function-code`, which does not overwrite Lambda environment variables.
- Token is supplied once during initial Lambda creation or later via local AWS CLI config update.

Optional future hardening:

- Store token in AWS Secrets Manager, e.g. `slack-holiday-status/slack-user-token`.
- Add code to fetch secret at runtime.
- Add `secretsmanager:GetSecretValue` permission to Lambda execution role.

This is not required for the first Lambda migration.

---

## 3. Go application changes needed

### Add Lambda dependency

Add dependency:

```bash
go get github.com/aws/aws-lambda-go/lambda
```

Expected `go.mod` addition:

```go
require github.com/aws/aws-lambda-go vX.Y.Z
```

### Entry point design

Keep local CLI behavior working, while enabling Lambda handler mode.

Detect Lambda by `AWS_LAMBDA_RUNTIME_API`, which Lambda sets automatically.

Implementation shape:

```go
package main

import (
    "context"
    "log/slog"
    "os"

    "github.com/aws/aws-lambda-go/lambda"
)

func main() {
    slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

    if os.Getenv("AWS_LAMBDA_RUNTIME_API") == "" {
        if err := run(context.Background()); err != nil {
            slog.Error("fatal error", "err", err)
            os.Exit(1)
        }
        return
    }

    lambda.Start(func(ctx context.Context) error {
        return run(ctx)
    })
}
```

### Thread context through `run`

Change:

```go
func run() error
```

To:

```go
func run(ctx context.Context) error
```

Change holiday fetch to accept context:

```go
func fetchHoliday(ctx context.Context, client *http.Client) (*nextHoliday, error)
```

Use request with context:

```go
req, err := http.NewRequestWithContext(ctx, http.MethodGet, diafestivoURL, nil)
if err != nil {
    return nil, fmt.Errorf("create request: %w", err)
}

resp, err := client.Do(req)
```

### Slack calls

The current Slack client calls can remain as-is initially:

- `api.GetUserProfile(...)`
- `api.SetUserCustomStatus(...)`
- `api.SetSnooze(...)`

The existing `http.Client{Timeout: httpTimeout}` still bounds individual Slack/holiday requests.

### Logging

Keep JSON logs using `slog` to stdout. Lambda automatically ships stdout/stderr to CloudWatch Logs when the function role has `AWSLambdaBasicExecutionRole`.

### Behavior parity requirements

Lambda behavior should match current Cloud Run behavior:

- If `SLACK_USER_TOKEN` missing: error, fail invocation.
- If holiday API fails: error, fail invocation.
- If not holiday: log and return nil.
- If holiday and Slack status already exists: log and return nil.
- If holiday and status is clear: set status and DND, log success.

---

## 4. Lambda build/package details

For `provided.al2023` zip deployment:

- Binary must be named `bootstrap`.
- `bootstrap` must be at the root of the zip file.
- Build for Linux.
- Prefer `arm64` for AWS Graviton cost/perf.
- Use `lambda.norpc` build tag for the custom runtime path.

Build command:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -tags lambda.norpc -ldflags="-s -w" -o bootstrap .

zip function.zip bootstrap
```

Generated local artifacts:

```text
bootstrap
function.zip
```

These should be gitignored if not already covered.

---

## 5. Local manual deployment script

Add a script, likely:

```text
deploy-lambda.sh
```

Purpose:

- Build `bootstrap`
- Create `function.zip`
- If Lambda function exists, update function code
- If Lambda function does not exist, optionally print create command or create it based on script mode

Recommended simple first version: update-only with helpful failure message.

### Suggested script behavior

Variables:

```bash
REGION="us-east-1"
FUNCTION_NAME="slack-holiday-status"
ARCH="arm64"
RUNTIME="provided.al2023"
ZIP_FILE="function.zip"
```

Steps:

1. Check required CLIs:
   - `go`
   - `zip`
   - `aws`
2. Build package:

```bash
rm -f bootstrap function.zip
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -tags lambda.norpc -ldflags="-s -w" -o bootstrap .
zip function.zip bootstrap
```

3. Verify current AWS caller:

```bash
aws sts get-caller-identity
```

4. Check if Lambda exists:

```bash
aws lambda get-function \
  --function-name "$FUNCTION_NAME" \
  --region "$REGION"
```

5. If exists, deploy:

```bash
aws lambda update-function-code \
  --function-name "$FUNCTION_NAME" \
  --zip-file "fileb://$ZIP_FILE" \
  --region "$REGION"
```

6. Optionally wait until update completes:

```bash
aws lambda wait function-updated \
  --function-name "$FUNCTION_NAME" \
  --region "$REGION"
```

7. Print success and manual invoke command:

```bash
aws lambda invoke \
  --function-name slack-holiday-status \
  --region us-east-1 \
  response.json
cat response.json
```

### Optional create mode

Later script can support:

```bash
./deploy-lambda.sh --create
```

But creation requires `SLACK_USER_TOKEN`, role ARN, and env var decisions. It may be cleaner to keep initial infra creation as documented AWS CLI commands and use script only for code deploys.

---

## 6. AWS infrastructure setup details

All commands assume:

```bash
ACCOUNT_ID=745912973548
REGION=us-east-1
FUNCTION_NAME=slack-holiday-status
```

### 6.1 Lambda execution role

Role name:

```text
slack-holiday-status-lambda-role
```

Trust policy file, e.g. `/tmp/lambda-trust.json`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "lambda.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
```

Create role:

```bash
aws iam create-role \
  --role-name slack-holiday-status-lambda-role \
  --assume-role-policy-document file:///tmp/lambda-trust.json
```

Attach basic logging policy:

```bash
aws iam attach-role-policy \
  --role-name slack-holiday-status-lambda-role \
  --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole
```

Capture role ARN:

```bash
LAMBDA_ROLE_ARN=$(aws iam get-role \
  --role-name slack-holiday-status-lambda-role \
  --query 'Role.Arn' \
  --output text)
```

### 6.2 Create Lambda function

Build the zip first:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -tags lambda.norpc -ldflags="-s -w" -o bootstrap .
zip function.zip bootstrap
```

Create function:

```bash
aws lambda create-function \
  --function-name slack-holiday-status \
  --runtime provided.al2023 \
  --handler bootstrap \
  --architectures arm64 \
  --role "$LAMBDA_ROLE_ARN" \
  --zip-file fileb://function.zip \
  --timeout 60 \
  --memory-size 128 \
  --region us-east-1 \
  --environment "Variables={SLACK_USER_TOKEN=$SLACK_USER_TOKEN}"
```

Notes:

- `SLACK_USER_TOKEN` should be exported locally before running the create command.
- Do not commit or print the token.
- 60 second timeout matches current Cloud Run job timeout.
- 128 MB memory should be sufficient for this small Go binary.

### 6.3 Manual Lambda invocation test

Invoke:

```bash
aws lambda invoke \
  --function-name slack-holiday-status \
  --region us-east-1 \
  response.json

cat response.json
```

Check logs:

```bash
aws logs tail /aws/lambda/slack-holiday-status \
  --region us-east-1 \
  --since 10m \
  --follow
```

Expected non-holiday log resembles:

```json
{"level":"INFO","msg":"not a holiday today","next":"...","days_until":3}
```

---

## 7. Automatic schedule setup

Use **EventBridge Scheduler**, not classic EventBridge Rule, because Scheduler supports time zones directly.

Schedule details:

```text
Name: slack-holiday-status-daily
Schedule: cron(0 2 * * ? *)
Timezone: America/Bogota
Target: Lambda function slack-holiday-status
Flexible window: OFF
Retry attempts: 3
```

### 7.1 Scheduler invoke role

Role name:

```text
slack-holiday-status-scheduler-role
```

Trust policy file, e.g. `/tmp/scheduler-trust.json`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "scheduler.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
```

Create role:

```bash
aws iam create-role \
  --role-name slack-holiday-status-scheduler-role \
  --assume-role-policy-document file:///tmp/scheduler-trust.json
```

Create invoke policy file, e.g. `/tmp/scheduler-invoke-policy.json`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "lambda:InvokeFunction",
      "Resource": "arn:aws:lambda:us-east-1:745912973548:function:slack-holiday-status"
    }
  ]
}
```

Attach inline policy:

```bash
aws iam put-role-policy \
  --role-name slack-holiday-status-scheduler-role \
  --policy-name invoke-slack-holiday-status-lambda \
  --policy-document file:///tmp/scheduler-invoke-policy.json
```

Capture scheduler role ARN:

```bash
SCHEDULER_ROLE_ARN=$(aws iam get-role \
  --role-name slack-holiday-status-scheduler-role \
  --query 'Role.Arn' \
  --output text)
```

### 7.2 Create EventBridge Scheduler schedule

Capture Lambda ARN:

```bash
LAMBDA_ARN=$(aws lambda get-function \
  --function-name slack-holiday-status \
  --region us-east-1 \
  --query 'Configuration.FunctionArn' \
  --output text)
```

Create schedule:

```bash
aws scheduler create-schedule \
  --name slack-holiday-status-daily \
  --region us-east-1 \
  --schedule-expression 'cron(0 2 * * ? *)' \
  --schedule-expression-timezone 'America/Bogota' \
  --flexible-time-window '{"Mode":"OFF"}' \
  --target "{\"Arn\":\"$LAMBDA_ARN\",\"RoleArn\":\"$SCHEDULER_ROLE_ARN\",\"RetryPolicy\":{\"MaximumRetryAttempts\":3}}"
```

Verify:

```bash
aws scheduler get-schedule \
  --name slack-holiday-status-daily \
  --region us-east-1
```

List matching schedules:

```bash
aws scheduler list-schedules \
  --region us-east-1 \
  --query "Schedules[?contains(Name, 'slack') || contains(Name, 'holiday')].[Name,GroupName,ScheduleExpression,State,Target.Arn]" \
  --output table
```

---

## 8. GitHub Actions deployment without static AWS keys

Use GitHub Actions OIDC to assume an AWS IAM role.

No `AWS_ACCESS_KEY_ID` or `AWS_SECRET_ACCESS_KEY` required.

### 8.1 Existing OIDC provider

Already exists in account:

```text
arn:aws:iam::745912973548:oidc-provider/token.actions.githubusercontent.com
```

Provider details observed:

```text
URL: token.actions.githubusercontent.com
Client ID: sts.amazonaws.com
```

### 8.2 Create dedicated GitHub deploy role

Role name:

```text
githubactions-slack-holiday-status-lambda
```

Trust should be limited to this exact repository and branch:

```text
repo:SomeoneWithOptions/slack-holiday-status:ref:refs/heads/lambda
```

Trust policy file, e.g. `/tmp/github-actions-trust.json`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::745912973548:oidc-provider/token.actions.githubusercontent.com"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
          "token.actions.githubusercontent.com:sub": "repo:SomeoneWithOptions/slack-holiday-status:ref:refs/heads/lambda"
        }
      }
    }
  ]
}
```

Create role:

```bash
aws iam create-role \
  --role-name githubactions-slack-holiday-status-lambda \
  --assume-role-policy-document file:///tmp/github-actions-trust.json
```

### 8.3 Attach least-privilege deploy permissions

Policy assumes Lambda already exists. GitHub Actions only updates code.

Policy file, e.g. `/tmp/github-actions-lambda-deploy-policy.json`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "UpdateSlackHolidayLambdaCode",
      "Effect": "Allow",
      "Action": [
        "lambda:GetFunction",
        "lambda:GetFunctionConfiguration",
        "lambda:UpdateFunctionCode"
      ],
      "Resource": "arn:aws:lambda:us-east-1:745912973548:function:slack-holiday-status"
    }
  ]
}
```

Attach inline policy:

```bash
aws iam put-role-policy \
  --role-name githubactions-slack-holiday-status-lambda \
  --policy-name deploy-slack-holiday-status-lambda-code \
  --policy-document file:///tmp/github-actions-lambda-deploy-policy.json
```

Capture role ARN:

```bash
GITHUB_ACTIONS_ROLE_ARN=$(aws iam get-role \
  --role-name githubactions-slack-holiday-status-lambda \
  --query 'Role.Arn' \
  --output text)
```

Expected ARN:

```text
arn:aws:iam::745912973548:role/githubactions-slack-holiday-status-lambda
```

### 8.4 GitHub repository variables

In GitHub:

```text
Repository → Settings → Secrets and variables → Actions → Variables
```

Add variables:

```text
AWS_REGION=us-east-1
AWS_ROLE_ARN=arn:aws:iam::745912973548:role/githubactions-slack-holiday-status-lambda
LAMBDA_FUNCTION_NAME=slack-holiday-status
```

No GitHub secret is needed for AWS authentication.

Do not add `SLACK_USER_TOKEN` to GitHub initially. Keep it only on the Lambda function environment.

---

## 9. GitHub Actions workflow

Add file:

```text
.github/workflows/deploy-lambda.yml
```

Workflow should run only on pushes to `lambda` branch.

Required permissions:

```yaml
permissions:
  id-token: write
  contents: read
```

`id-token: write` is required for GitHub OIDC.

Suggested workflow:

```yaml
name: Deploy Lambda

on:
  push:
    branches:
      - lambda

permissions:
  id-token: write
  contents: read

jobs:
  deploy:
    runs-on: ubuntu-latest

    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: "1.26.3"
          cache: true

      - name: Test
        run: go test ./...

      - name: Build Lambda zip
        run: |
          CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
            go build -tags lambda.norpc -ldflags="-s -w" -o bootstrap .
          zip function.zip bootstrap

      - name: Configure AWS credentials
        uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: ${{ vars.AWS_ROLE_ARN }}
          aws-region: ${{ vars.AWS_REGION }}

      - name: Deploy Lambda code
        run: |
          aws lambda update-function-code \
            --function-name "${{ vars.LAMBDA_FUNCTION_NAME }}" \
            --zip-file fileb://function.zip

      - name: Wait for update
        run: |
          aws lambda wait function-updated \
            --function-name "${{ vars.LAMBDA_FUNCTION_NAME }}"
```

Optional improvement:

- After deploy, call `aws lambda get-function-configuration` and print `LastModified`, `Runtime`, `Architectures`, and `CodeSha256`.
- Avoid invoking automatically from CI because on a holiday it can modify Slack status/DND. If desired, add manual `workflow_dispatch` with an explicit deploy/test flag.

---

## 10. Initial setup order

Recommended implementation sequence:

1. Update Go app for Lambda compatibility:
   - add `aws-lambda-go/lambda`
   - change `main()` to support CLI and Lambda
   - thread `context.Context` through `run` and `fetchHoliday`

2. Add local Lambda build/deploy script:
   - `deploy-lambda.sh`
   - builds `bootstrap`
   - zips `function.zip`
   - updates existing function

3. Add generated artifacts to `.gitignore`:
   - `bootstrap`
   - `function.zip`
   - possibly `response.json`

4. Create AWS Lambda execution role.

5. Build zip locally.

6. Create Lambda function with `SLACK_USER_TOKEN` env var.

7. Manually invoke Lambda once and check CloudWatch logs.

8. Create Scheduler invoke role.

9. Create EventBridge Scheduler schedule.

10. Create GitHub Actions OIDC deploy role.

11. Add GitHub repo variables.

12. Add GitHub Actions workflow.

13. Push to `lambda` branch and verify deploy.

---

## 11. Verification checklist

### Local build

```bash
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap .
zip function.zip bootstrap
```

### Lambda function exists

```bash
aws lambda get-function \
  --function-name slack-holiday-status \
  --region us-east-1
```

### Lambda env var exists without exposing value

```bash
aws lambda get-function-configuration \
  --function-name slack-holiday-status \
  --region us-east-1 \
  --query 'Environment.Variables | keys(@)'
```

Expected includes:

```text
SLACK_USER_TOKEN
```

### Manual invoke

```bash
aws lambda invoke \
  --function-name slack-holiday-status \
  --region us-east-1 \
  response.json
```

### Logs

```bash
aws logs tail /aws/lambda/slack-holiday-status \
  --region us-east-1 \
  --since 10m
```

### Schedule

```bash
aws scheduler get-schedule \
  --name slack-holiday-status-daily \
  --region us-east-1
```

### GitHub Actions role trust

```bash
aws iam get-role \
  --role-name githubactions-slack-holiday-status-lambda \
  --query 'Role.AssumeRolePolicyDocument'
```

Confirm it contains:

```text
repo:SomeoneWithOptions/slack-holiday-status:ref:refs/heads/lambda
```

### GitHub Actions deploy

Push to branch:

```bash
git push origin lambda
```

Expected:

- workflow runs
- OIDC assume role succeeds
- zip builds
- Lambda code updates
- no static AWS key involved

---

## 12. Notes / future improvements

### Secrets Manager

If moving Slack token out of Lambda env vars later:

1. Create secret:

```bash
aws secretsmanager create-secret \
  --name slack-holiday-status/slack-user-token \
  --secret-string "$SLACK_USER_TOKEN" \
  --region us-east-1
```

2. Add IAM permission to Lambda role:

```json
{
  "Effect": "Allow",
  "Action": "secretsmanager:GetSecretValue",
  "Resource": "arn:aws:secretsmanager:us-east-1:745912973548:secret:slack-holiday-status/slack-user-token-*"
}
```

3. Change app to read secret from Secrets Manager instead of direct env token, likely using AWS SDK for Go v2.

### Infrastructure as code

Current plan uses AWS CLI commands for setup. Future cleanup could move AWS resources to Terraform, CloudFormation, or SAM.

For now, because the app has only a few resources, CLI setup plus documented commands is acceptable.

### Cloud Run files

Initially leave Cloud Run/GCP files in place unless explicitly deciding to make the repository AWS-only.

Possible later cleanup:

- keep `Dockerfile` as historical/alternate deploy path
- rename GCP-specific docs
- update README to make Lambda the primary deployment target
