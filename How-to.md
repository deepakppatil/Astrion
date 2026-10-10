# How to set up and run Astrion locally

This guide takes you from a fresh machine to a working Astrion: you'll submit
a job, answer the agent's question, and download the result. It covers
**macOS**, **Linux**, and **Windows**. Every step has a command block per
operating system; use the one for yours.

Shorter, platform-specific versions of this guide:

- [macOS](docs/getting-started/local-macos.md)
- [Windows](docs/getting-started/local-windows.md)

---

## What you will run

```
            ┌──────────────┐  SQL   ┌────────────┐
 Browser ──►│ astrion-api  │───────►│ PostgreSQL │
  :8080     │ (+ web UI)   │◄──┐    └────────────┘
            └──────┬───────┘   │ internal API
                   │ start     │
                   ▼           │
            ┌──────────────┐   │
            │   Temporal   │   │
            │  dev server  │   │
            └──────┬───────┘   │
                   │ tasks     │
                   ▼           │
            ┌──────────────┐   │    ┌─────────────────────────────┐
            │astrion-worker│───┴───►│ astrion-supervisor          │
            └──────────────┘ spawns │   └─ mock-agent (fake AI)   │
                                    └─────────────────────────────┘
```

| Process | Port | What it does |
|---|---|---|
| PostgreSQL | 5432 | Stores jobs, interactions, events, audit |
| Temporal dev server | 7233 (UI 8233) | Runs each job as a durable workflow |
| `astrion-api` | 8080 | REST API, live updates, serves the web console |
| `astrion-worker` | — | Runs workflows; launches each job as a local process |
| Vite dev server (optional) | 5173 | Hot-reloading console, for UI work |

Everything runs on your machine. You don't need Docker, Kubernetes, or an AI
subscription, because jobs use the built-in **mock agent** on the **local
provider**. Both are development-only, and production mode refuses them.

Time: about 15–20 minutes.

---

## Step 1 — Install the prerequisites

| Tool | Minimum version |
|---|---|
| Go | 1.26 |
| Node.js | 22.12 (or 20.19) |
| PostgreSQL | 15 (17 recommended) |
| Temporal CLI | 1.x |
| Git | 2.30 |

### macOS

Install [Homebrew](https://brew.sh) if you don't have it, then:

```bash
brew install go node postgresql@17 temporal git

# postgresql@17 is keg-only: put its tools on PATH
echo 'export PATH="$(brew --prefix postgresql@17)/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

### Linux (Debian/Ubuntu shown; adapt the package manager for others)

Distribution Go packages are usually too old, so install Go from
<https://go.dev/dl/>:

```bash
# Go: replace the file name with the latest 1.26+ release for your CPU
curl -LO https://go.dev/dl/go1.26.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.0.linux-amd64.tar.gz
echo 'export PATH="/usr/local/go/bin:$PATH"' >> ~/.bashrc

# Node.js 22 via nvm
curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash
source ~/.bashrc && nvm install 22

# PostgreSQL and Git
sudo apt update && sudo apt install -y postgresql git

# Temporal CLI (installs to ~/.temporalio/bin)
curl -sSf https://temporal.download/cli.sh | sh
echo 'export PATH="$HOME/.temporalio/bin:$PATH"' >> ~/.bashrc

source ~/.bashrc
```

### Windows (PowerShell)

```powershell
winget install -e --id GoLang.Go
winget install -e --id OpenJS.NodeJS.LTS
winget install -e --id Git.Git
winget install -e --id Temporal.TemporalCLI
winget install -e --id PostgreSQL.PostgreSQL.17 --interactive
```

The PostgreSQL installer asks you to choose a password for the `postgres`
user. Remember it. Then **close and reopen PowerShell** so the new `PATH`
takes effect.

> Windows support is a **preview**. It is compiled and vetted, but the
> maintainers have not yet run it end to end on Windows. Cancelling a job
> force-kills the agent immediately, and private-repository credentials are
> untested. WSL 2 with the Linux instructions also works.

### Check your tools (all platforms)

```bash
go version          # go1.26 or newer
node --version      # v22.12 or newer
git --version
temporal --version
```

---

## Step 2 — Get the code and build it

macOS / Linux:

```bash
git clone https://github.com/deepakppatil/Astrion.git
cd Astrion

go build -o bin/ ./cmd/...                    # Go binaries into ./bin
(cd web && npm ci && npm run build)           # web console into ./web/dist
```

Windows:

```powershell
git clone https://github.com/deepakppatil/Astrion.git
cd Astrion

go build -o bin/ ./cmd/...                    # .exe files into .\bin
cd web; npm ci; npm run build; cd ..          # web console into .\web\dist
```

You should now have four programs in `bin/`:

- `astrion-api`;
- `astrion-worker`;
- `astrion-supervisor`;
- `mock-agent`.

On Windows each has an `.exe` suffix.

> **All remaining commands are run from the repository root** (the `Astrion`
> folder). The API and worker share the `.astrion/` folder there for
> artifacts.

---

## Step 3 — Create the database

### macOS

```bash
brew services start postgresql@17
createdb astrion
```

Homebrew trusts local connections for your user, so no password is needed.

```
DATABASE URL:  postgres://<your-mac-username>@localhost:5432/astrion?sslmode=disable
```

### Linux

Ubuntu's PostgreSQL requires a password for `localhost` connections, so
create a dedicated role:

```bash
sudo systemctl enable --now postgresql
sudo -u postgres psql -c "CREATE ROLE astrion LOGIN PASSWORD 'astrion' CREATEDB"
sudo -u postgres createdb -O astrion astrion
```

`CREATEDB` is only needed if you also want to run the database tests (Step 10).

```
DATABASE URL:  postgres://astrion:astrion@localhost:5432/astrion?sslmode=disable
```

### Windows

PostgreSQL runs as a Windows service after installation. Create the database
and enter the `postgres` password when asked:

```powershell
& "C:\Program Files\PostgreSQL\17\bin\createdb.exe" -U postgres -h localhost astrion
```

```
DATABASE URL:  postgres://postgres:<your-password>@localhost:5432/astrion?sslmode=disable
```

If the password contains special characters, URL-encode them (`@` → `%40`,
`:` → `%3A`).

You do **not** need to create tables. The API and worker run the database
migrations automatically on start.

---

## Step 4 — Create a demo repository

Almost every command works on code, so jobs need a Git repository. Only
`/Create-Jira` runs without one; anything else is rejected with
*"command … requires at least one repository"*. A local repository is fine.

macOS / Linux:

```bash
mkdir -p ~/astrion-demo && cd ~/astrion-demo
git init -b main
echo "# Demo" > README.md
git add . && git -c user.name=demo -c user.email=demo@example.com commit -m init
cd -
echo "file://$HOME/astrion-demo"     # you need this URL in the next step
```

Windows:

```powershell
New-Item -ItemType Directory $HOME\astrion-demo | Out-Null
Push-Location $HOME\astrion-demo
git init -b main
"# Demo" | Out-File -Encoding utf8 README.md
git add .; git -c user.name=demo -c user.email=demo@example.com commit -m init
Pop-Location
"file:///" + ((Resolve-Path $HOME\astrion-demo).Path -replace '\\', '/')   # you need this URL in the next step
```

Copy the printed `file://…` URL. On every start, the API attaches it to the
seeded demo workspace if it is missing.

---

## Step 5 — Save your settings in a file

You will open three terminals. A small settings file saves retyping.

macOS / Linux: create `dev.env` in the repository root:

```bash
# dev.env — load with:  set -a; source dev.env; set +a
ASTRION_DATABASE_URL=postgres://astrion:astrion@localhost:5432/astrion?sslmode=disable   # use YOUR URL from step 3
ASTRION_HTTP_ADDR=127.0.0.1:8080
ASTRION_WEB_DIR=web/dist
ASTRION_DEMO_REPO_URL=file:///home/you/astrion-demo                                      # the URL printed in step 4
```

Windows: create `dev-env.ps1` in the repository root:

```powershell
# dev-env.ps1 — load with:  . .\dev-env.ps1
$env:ASTRION_DATABASE_URL = "postgres://postgres:YOUR_PASSWORD@localhost:5432/astrion?sslmode=disable"
$env:ASTRION_HTTP_ADDR    = "127.0.0.1:8080"
$env:ASTRION_WEB_DIR      = "web\dist"
$env:Path                 = "$PWD\bin;$env:Path"
$env:ASTRION_DEMO_REPO_URL = "file:///C:/Users/<you>/astrion-demo"   # the URL printed in step 4
```

Both files contain a password, so keep them out of Git. `.env` files are
already ignored. Either add `dev.env` and `dev-env.ps1` to `.git/info/exclude`
or name the file `.env` instead.

---

## Step 6 — Start Temporal (terminal 1)

```bash
temporal server start-dev --db-filename temporal.db
```

Leave it running.

- It listens on `localhost:7233`, which Astrion uses by default.
- Its web UI at <http://localhost:8233> shows every Astrion job as a workflow
  named `job-<jobId>`.
- `--db-filename` keeps history across restarts. Omit it for a throwaway
  in-memory server.

---

## Step 7 — Start the API (terminal 2)

macOS / Linux:

```bash
set -a; source dev.env; set +a
./bin/astrion-api
```

Windows:

```powershell
. .\dev-env.ps1
.\bin\astrion-api.exe
```

On first start the API migrates the database and seeds the demo data:

- a default tenant;
- the SDD framework with its commands;
- a mock agent profile;
- a demo workspace.

The last log line reads `"msg":"listening"`.

Check it from another terminal:

```bash
curl http://127.0.0.1:8080/readyz                    # {"status":"ready"}
```

```powershell
Invoke-RestMethod http://127.0.0.1:8080/readyz       # status: ready
```

---

## Step 8 — Start the worker (terminal 3)

The worker launches `astrion-supervisor` and `mock-agent` for each job, so
`bin/` **must be on your `PATH`**. On Windows, `dev-env.ps1` already does this.

macOS / Linux:

```bash
set -a; source dev.env; set +a
PATH="$PWD/bin:$PATH" ./bin/astrion-worker
```

Windows:

```powershell
. .\dev-env.ps1
.\bin\astrion-worker.exe
```

The worker registers the Temporal namespace on first start. It logs
`"msg":"worker started"` when ready.

---

## Step 9 — Use it

### Sign in

Open **<http://127.0.0.1:8080>** and pick a development persona:

| Persona | Role | Good for |
|---|---|---|
| **Bea** | Business analyst / PM | Plan commands (`/write-spec`, `/Plan-migrate`, …) |
| **Arch** | Architect | Design commands |
| **Dana** | Developer + QA | Implement and Test commands |
| **Rel** | Release / platform engineer | Deploy commands, production approvals |
| **Admin** | Platform administrator | Workspaces, registry, audit (cannot run delivery commands) |

### Run your first job (with human-in-the-loop)

1. Sign in as **Bea** and open **Run command**.
2. Choose `/write-spec`, type a spec name (for example `Checkout flow`), and
   submit.
3. The job page updates live as the job moves through `QUEUED` →
   `PROVISIONING` → `PREPARING_WORKSPACE` → `RUNNING`. Locally this takes
   about a second, so some states may flash by. The **Activity** tab lists
   every transition.
4. The mock agent asks *"/write-spec: which level of detail should the output
   target?"*, and the job switches to **`WAITING_FOR_INPUT`**. Its process is
   released while it waits, so nothing keeps running.
5. Answer on the job page, or from the **Inbox**. Pick *Standard*.
6. The job resumes from its checkpoint, uploads a `specification.md`
   artifact, cleans up, and ends **`SUCCEEDED`**. Open the artifact to read
   it.

### Try other behaviours

Put one of these tags anywhere in an input (for example in the spec name):

| Tag | What happens |
|---|---|
| `[mock:no-hitl]` | Finishes without asking anything |
| `[mock:approval]` | Asks for an **approval** instead of a question |
| `[mock:fail]` | Fails; try **Retry** afterwards |
| `[mock:slow]` | Runs slowly; try **Cancel** |
| `[mock:secret-leak]` | Prints a fake token; check that the logs show it redacted |
| `[mock:skip-artifact]` | "Succeeds" without the required output; the job fails validation |

### Use the API directly

macOS / Linux:

```bash
TOKEN=dev-analyst-token
WS=$(curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/workspaces \
     | python3 -c 'import sys,json; print(json.load(sys.stdin)["items"][0]["id"])')

curl -s -X POST http://127.0.0.1:8080/api/v1/jobs \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-$(date +%s)" \
  -d "{\"workspaceId\":\"$WS\",\"command\":\"/write-spec\",\"inputs\":{\"specName\":\"API demo [mock:no-hitl]\"}}"
```

Windows:

```powershell
$h  = @{ Authorization = "Bearer dev-analyst-token"; "Idempotency-Key" = "demo-$(Get-Date -UFormat %s)" }
$ws = (Invoke-RestMethod http://127.0.0.1:8080/api/v1/workspaces -Headers $h).items[0].id
$body = @{ workspaceId = $ws; command = "/write-spec"; inputs = @{ specName = "API demo [mock:no-hitl]" } } | ConvertTo-Json
Invoke-RestMethod -Method Post http://127.0.0.1:8080/api/v1/jobs -Headers $h -ContentType application/json -Body $body
```

The response contains the job `id`. Follow it with
`GET /api/v1/jobs/{id}` or stream live with `GET /api/v1/jobs/{id}/events`.
The full API is in [`api/openapi.yaml`](api/openapi.yaml).

---

## Step 10 — Everyday development

### Work on the web console (hot reload)

Keep the API and worker running, then:

```bash
cd web
npm run dev          # http://localhost:5173, proxies /api to 127.0.0.1:8080
```

If your API runs elsewhere, set `ASTRION_API_URL` before `npm run dev`.

### After changing Go code

Rebuild, then restart the API and/or worker (`Ctrl+C`, then start again):

```bash
go build -o bin/ ./cmd/...
```

Jobs survive a restart: Temporal keeps their state, and they continue when
the worker is back.

### Run the tests

```bash
# Go unit tests (database tests are skipped)
go test ./...

# Include database tests: point at a role that may CREATE DATABASE
#   macOS:  postgres://<you>@localhost:5432/postgres?sslmode=disable
#   Linux:  postgres://astrion:astrion@localhost:5432/postgres?sslmode=disable
export ASTRION_TEST_DATABASE_URL="postgres://astrion:astrion@localhost:5432/postgres?sslmode=disable"
go test ./...

# Static checks, including the Windows and macOS builds
go vet ./... && GOOS=windows go vet ./... && GOOS=darwin go vet ./...

# Web console
cd web && npx tsc -b && npm test
```

On Windows use `$env:ASTRION_TEST_DATABASE_URL = "postgres://postgres:YOUR_PASSWORD@localhost:5432/postgres?sslmode=disable"`.

---

## Step 11 — Stop and reset

**Stop:** press `Ctrl+C` in each terminal (worker, API, Temporal).

- On macOS, `brew services stop postgresql@17` stops the database.
- On Linux, use `sudo systemctl stop postgresql`.

**Start over from scratch:** stop everything, then:

macOS / Linux:

```bash
dropdb astrion && createdb astrion            # Linux: sudo -u postgres dropdb astrion && sudo -u postgres createdb -O astrion astrion
rm -rf .astrion temporal.db
```

Windows:

```powershell
& "C:\Program Files\PostgreSQL\17\bin\dropdb.exe"   -U postgres -h localhost astrion
& "C:\Program Files\PostgreSQL\17\bin\createdb.exe" -U postgres -h localhost astrion
Remove-Item -Recurse -Force .astrion, temporal.db
```

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `createdb: command not found` (macOS) | The keg-only `PATH` line from Step 1 is missing. |
| API exits: `connect: connection refused` on 5432 | PostgreSQL isn't running. Start it (Step 3). |
| `password authentication failed` | Wrong password in the database URL, or special characters need URL-encoding. |
| Job rejected: `command … requires at least one repository` | `ASTRION_DEMO_REPO_URL` wasn't set when the API started. Set it (Steps 4–5) and restart the API. |
| API exits: `ASTRION_DATABASE_URL is required` | Settings weren't loaded in this terminal; run the `source` / dot-source line again. |
| Worker logs `failed reaching server` | Temporal (terminal 1) isn't running. |
| Job stuck in `PROVISIONING`; worker log shows `executable file not found` | `bin/` isn't on the worker's `PATH` (Step 8). |
| Artifact download returns 404 | API and worker were started from different folders. Run both from the repository root, or set the same absolute `ASTRION_DATA_DIR` for both. |
| Port 8080 already in use | Change `ASTRION_HTTP_ADDR`, **and** set `ASTRION_EXEC_API_URL=http://127.0.0.1:<port>` for the worker. |
| <http://127.0.0.1:8080> shows `404 page not found` | `ASTRION_WEB_DIR` isn't set in the API's terminal, or `web/dist` wasn't built (`npm run build` in `web/`, Step 2). |
| A command isn't listed under **Run command** | Your persona lacks the permissions it requires; for example, Admin cannot run delivery commands. The **Registry** page shows each command's personas. For `/write-spec`, use **Bea**. |
| "commands are case-sensitive; did you mean …" | Command names keep exact casing: `/Plan-migrate`, `/Create-Jira`. |
| Windows: `npm.ps1 cannot be loaded` | Use `npm.cmd`, or run `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`. |
| Windows: `go`/`node`/`temporal` not recognized | Open a new PowerShell window after installing. |
| macOS/Windows firewall prompt | Allow on private networks; with `127.0.0.1` addresses nothing is exposed. |

---

## Reference: environment variables used here

| Variable | Default | Meaning |
|---|---|---|
| `ASTRION_DATABASE_URL` | — (required) | PostgreSQL connection string |
| `ASTRION_HTTP_ADDR` | `:8080` (all interfaces) | API listen address; the guide uses `127.0.0.1:8080` |
| `ASTRION_WEB_DIR` | — | Folder with the built console (`web/dist`) |
| `ASTRION_DEMO_REPO_URL` | — | Repository attached to the demo workspace |
| `ASTRION_DATA_DIR` | `.astrion` | Local artifacts and job work folders |
| `ASTRION_EXEC_API_URL` | `http://127.0.0.1:8080` | How job processes reach the API (worker setting) |
| `ASTRION_TEMPORAL_ADDRESS` | `127.0.0.1:7233` | Temporal server |
| `ASTRION_TEMPORAL_NAMESPACE` | `default` | Temporal namespace |
| `ASTRION_LOG_LEVEL` | `info` | `debug` for more detail |

The full list, including OIDC, S3 and Kubernetes settings for real
deployments, is in the [README](README.md#configuration) and
[`internal/config/config.go`](internal/config/config.go).

## Where to go next

- [Architecture overview](docs/architecture/overview.md) — how the pieces fit together.
- [Job state machine](docs/architecture/state-machine.md) — every state and transition.
- [ADRs](docs/adr/README.md) — why it is built this way.
- [Implementation plan](docs/plan/implementation-plan.md) — what's done and what's next.
