# Run Astrion locally on Windows

This guide runs the complete vertical slice on one Windows 10/11 machine:

- PostgreSQL;
- a Temporal dev server;
- the API;
- the worker;
- the web console;
- the **mock agent** on the **local provider**.

Jobs run as local processes, and no Docker, WSL, or Kubernetes is needed.
All commands are for **PowerShell**.

> **Support status: preview.** Windows binaries are cross-compiled and
> `go vet`-clean, and process control has a Windows implementation
> (`internal/procgroup`). The maintainers have not yet run the full flow on
> Windows. Known differences:
>
> - **Cancellation is forceful.** Windows has no SIGTERM, so cancelling a
>   job ends the agent's process tree with `taskkill /T /F` immediately,
>   instead of a graceful stop followed by a kill.
> - **Private-repository credentials are untested.** Credentials reach Git
>   through a POSIX `GIT_ASKPASS` script. Local `file://` and public
>   repositories don't need credentials.
>
> Please report problems as GitHub issues. If you prefer a Linux
> environment, run Astrion inside WSL 2 using the
> [Linux notes](../../README.md#linux) instead.

## 1. Install prerequisites

With `winget` (built into Windows 10 21H2+ and 11):

```powershell
winget install -e --id GoLang.Go
winget install -e --id OpenJS.NodeJS.LTS
winget install -e --id Git.Git
winget install -e --id Temporal.TemporalCLI
winget install -e --id PostgreSQL.PostgreSQL.17 --interactive
```

The PostgreSQL installer asks for a password for the `postgres` superuser.
Remember it. You can untick *Stack Builder*.

**Open a new PowerShell window** so the updated `PATH` takes effect, then check:

| Tool | Minimum | Check |
|---|---|---|
| Go | 1.26 | `go version` |
| Node.js | 22.12 | `node --version` |
| Git | 2.30+ | `git --version` |
| Temporal CLI | 1.x | `temporal --version` |
| PostgreSQL | 15+ (17 recommended) | `& "C:\Program Files\PostgreSQL\17\bin\postgres.exe" --version` |

## 2. Get the code and build

```powershell
git clone https://github.com/deepakppatil/Astrion.git
cd Astrion

go build -o bin/ ./cmd/...      # astrion-api.exe, astrion-worker.exe, astrion-supervisor.exe, mock-agent.exe

cd web; npm ci; npm run build; cd ..   # produces web\dist
```

## 3. Create the database

The installer registers PostgreSQL as a Windows service that starts
automatically. Create the database (enter the `postgres` password when
prompted):

```powershell
& "C:\Program Files\PostgreSQL\17\bin\createdb.exe" -U postgres -h localhost astrion
```

The connection string, used in every terminal below, is:

```powershell
$env:ASTRION_DATABASE_URL = "postgres://postgres:YOUR_PASSWORD@localhost:5432/astrion?sslmode=disable"
```

URL-encode special characters in the password (for example `@` → `%40`).
The API and worker create the schema automatically on start.

## 4. Start Temporal (terminal 1)

```powershell
temporal server start-dev --db-filename temporal.db
```

- gRPC listens on `localhost:7233`; Astrion's default is `127.0.0.1:7233`.
- The Temporal Web UI is at <http://localhost:8233>. Every job appears as a
  workflow named `job-<jobId>`.
- If Windows Defender Firewall asks, allowing **private networks** is
  enough. Everything here uses loopback.

## 5. Optional: a demo repository

```powershell
New-Item -ItemType Directory $HOME\astrion-demo | Out-Null
Push-Location $HOME\astrion-demo
git init -b main
"# Demo" | Out-File -Encoding utf8 README.md
git add .; git -c user.name=demo -c user.email=demo@example.com commit -m init
Pop-Location

# file:///C:/Users/<you>/astrion-demo
$demo = "file:///" + ((Resolve-Path $HOME\astrion-demo).Path -replace '\\', '/')
$demo
```

## 6. Start the API (terminal 2)

Run from the repository root:

```powershell
$env:ASTRION_DATABASE_URL = "postgres://postgres:YOUR_PASSWORD@localhost:5432/astrion?sslmode=disable"
$env:ASTRION_HTTP_ADDR = "127.0.0.1:8080"
$env:ASTRION_WEB_DIR = "web\dist"
$env:ASTRION_DEMO_REPO_URL = "file:///C:/Users/<you>/astrion-demo"   # optional, from step 5
.\bin\astrion-api.exe
```

On first start it migrates the database and seeds:

- the default tenant;
- the SDD framework and its commands;
- a mock agent profile;
- a demo workspace.

Check it: `Invoke-RestMethod http://127.0.0.1:8080/readyz` → `status: ready`.

## 7. Start the worker (terminal 3)

The worker launches `astrion-supervisor.exe` and `mock-agent.exe` from your
`PATH`. Run it from the repository root so it shares `.astrion\` (the local
artifact store) with the API:

```powershell
$env:ASTRION_DATABASE_URL = "postgres://postgres:YOUR_PASSWORD@localhost:5432/astrion?sslmode=disable"
$env:Path = "$PWD\bin;$env:Path"
.\bin\astrion-worker.exe
```

In development the worker registers the Temporal namespace if needed. It
then polls the `astrion-jobs` task queue.

`$env:` variables only last for the current PowerShell window. To keep them,
put the lines in a script (for example `dev-env.ps1`) and dot-source it in
each terminal: `. .\dev-env.ps1`.

## 8. Use it

Open <http://127.0.0.1:8080> and pick a development persona:

- **Dana** (developer + QA);
- **Bea** (business analyst);
- **Arch** (architect);
- **Rel** (release engineer);
- **Admin**.

Try the human-in-the-loop flow:

1. Sign in as **Bea** and go to **Run command**. Choose `/write-spec`, enter
   a spec name, and submit.
2. The job streams live. The mock agent asks *"/write-spec: which level of
   detail should the output target?"*, and the job enters
   `WAITING_FOR_INPUT`.
3. Answer from the job page or the **Inbox**. The job resumes from its
   checkpoint, produces a `specification` artifact, and ends `SUCCEEDED`.

Add `[mock:no-hitl]`, `[mock:approval]`, `[mock:fail]`, `[mock:slow]`, or
`[mock:secret-leak]` to an input to change the mock agent's behaviour. The
[macOS guide](local-macos.md#8-use-it) explains each tag.

From PowerShell:

```powershell
$h = @{ Authorization = "Bearer dev-analyst-token"; "Idempotency-Key" = "demo-$(Get-Date -UFormat %s)" }
$ws = (Invoke-RestMethod http://127.0.0.1:8080/api/v1/workspaces -Headers $h).items[0].id
$body = @{ workspaceId = $ws; command = "/write-spec"; inputs = @{ specName = "Demo [mock:no-hitl]" } } | ConvertTo-Json
Invoke-RestMethod -Method Post http://127.0.0.1:8080/api/v1/jobs -Headers $h -ContentType application/json -Body $body
```

The full API is described in [`api/openapi.yaml`](../../api/openapi.yaml).

## Option: hot-reloading console

```powershell
cd web; npm run dev     # http://localhost:5173, proxies /api to 127.0.0.1:8080
```

## Stop and reset

- Stop each terminal with `Ctrl+C`. Temporal keeps job state, so in-flight
  jobs carry on when the worker is restarted.
- To start over:

  ```powershell
  & "C:\Program Files\PostgreSQL\17\bin\dropdb.exe" -U postgres -h localhost astrion
  & "C:\Program Files\PostgreSQL\17\bin\createdb.exe" -U postgres -h localhost astrion
  Remove-Item -Recurse -Force .astrion, temporal.db
  ```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `npm : File ...\npm.ps1 cannot be loaded because running scripts is disabled` | Run `npm.cmd` instead of `npm`, or allow local scripts for your user: `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned` |
| `go`, `node`, or `temporal` not recognized | Open a new PowerShell window after `winget install`. |
| `password authentication failed for user "postgres"` | Wrong password in `ASTRION_DATABASE_URL`, or it needs URL-encoding. |
| Worker logs `failed reaching server` | Temporal (terminal 1) isn't running. |
| Job stuck in `PROVISIONING`; worker log shows `executable file not found` | Run the worker with `bin` on `$env:Path` (§7). |
| Artifact download returns 404 | Start API and worker from the same directory, or give both the same absolute `ASTRION_DATA_DIR`. |
| Port 8080 in use | Change `ASTRION_HTTP_ADDR` and set `ASTRION_EXEC_API_URL` to match on the worker. |
| Antivirus quarantines `mock-agent.exe` or slows builds | Exclude the repository folder from real-time scanning. |
