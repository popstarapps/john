# John

**The safe, dependable coding agent for enterprises.**

John is an enterprise coding agent, like Claude Code or Codex, with centralized controls for authentication, authorization, sandboxing, network access, and LLM usage.

It is built for regulated and security-conscious organizations that want the benefits of AI-assisted development without giving up control of their source code, secrets, network, or data.

The agent loop runs in the backend, while the client holds the developer's session, executes tool calls inside a sandbox, and renders the session in the terminal. It holds no model credentials; the backend applies policy to every model call.

Commands that operate on a project run in a sandbox on the developer's machine. Outbound HTTP and HTTPS traffic from the sandbox passes through a local network proxy, which enforces centrally managed policies. Every LLM request is made by the backend, which applies replacements, model routing, and trace recording.

## Installation

1. **Download the backend.** From [GitHub Releases](https://github.com/popstarapps/john/releases), take the `john-<version>-full-<os>-<arch>` file for your server — the `full` ones, not the `clientonly` ones, which your developers get from the backend itself. Linux, macOS and Windows all have one. Rename it to `john`, make it executable, and put it on your `PATH`. It includes the web console and client downloads for all supported platforms.

2. **Run setup** from a directory with persistent storage:

   ```sh
   john backend --setup
   ```

   _Save the printed trace read key securely_; it is shown only once. Open the setup URL and configure your organization, OIDC provider, initial administrator, models, and trace directory. The database defaults to `john.db` in the current directory.

   Sign-in admits only the email domains you allow, which default to the administrator's own; change them under Settings → Identity. The identity provider says who someone is, and this is what keeps it from deciding who belongs: a Google client left on External would otherwise admit every Google account. Addresses the provider marks unverified are refused, and with Google (`https://accounts.google.com`) the account must also belong to the Workspace that owns the domain.

   Setup listens on `127.0.0.1:8080`, so on a server without a browser, reach it from your own computer. Either tunnel to it and open the printed URL unchanged:

   ```sh
   ssh -L 8080:127.0.0.1:8080 you@your-server
   ```

   or type the number of one of the server's other addresses, which setup lists when run in a terminal, to have it answer there as well. That runs setup over plain HTTP, and the form carries your OIDC client secret and model API keys, so prefer the tunnel on any network you do not trust. `--listen <address>:<port>` chooses the address up front, for setup and for the backend alike.

3. **Start the backend.** Stop the setup process, then run `john backend` from the same directory under your service manager. It listens on `127.0.0.1:8080`, or wherever `--listen` says. From here you can put an HTTPS reverse proxy in front of it that sets `X-Forwarded-Proto: https` and `X-Forwarded-Host` to your public hostname, register `https://<your-john-backend>/auth/callback` with your OIDC provider, and back up the database and trace directory as needed.

4. **Connect developers.** Sign in to the console to configure teams, policies, and model access. Developers download their client from the console, install it on their `PATH`, and run:

   ```sh
   john login https://<your-john-backend>
   cd path/to/project
   john
   ```

See [Requirements](#requirements) for workstation prerequisites.

## Architecture

John has three components:

- **The backend** runs the agent loop, the policy engine, and the model router. It is the only thing in the system that talks to a model provider.
- **The client** is a command-line application. It runs the tool calls the backend sends down — read a file, edit a file, run a command — inside the sandbox, and returns their output as data. It also runs the network proxy.
- **The sandbox** is a Linux virtual machine on the developer's machine. Tools execute there and nowhere else.

The client sends the prompt to the backend. When the agent requests a tool, the backend sends the call to the client, which executes it in the sandbox and returns the result. The backend uses that result to continue the turn.

## Client

John is a command-line application for macOS, Windows and Linux.

1. Download John.
2. Sign in with your organization's identity provider using `john login https://<your-john-backend>`. The backend authenticates through OIDC. The URL is needed once; the machine remembers it, and `john login` on its own signs in again where it already is.
3. Run John from a project directory.
4. John creates or resumes the Linux sandbox for the current directory.
   Sandboxes are built from `.john/Dockerfile` in the project, or from a built-in
   image when the project does not provide one.
5. The backend's agent loop reads, edits, and runs code through the client, and
   every one of those tool calls executes inside the sandbox. Edits land in the
   sandbox's copy-on-write layer, not in the host working directory.
6. The developer reviews the resulting diff and applies it to the host working
   directory.

Use `@` to mention a file in a prompt. The model reads text files with the sandbox's `read` tool, subject to workspace policies. Mentioned images and PDFs are attached to the prompt for the model to view.

Attachments inside the workspace are read through the sandbox, which enforces exclusions. An attachment outside the workspace, such as a screenshot in `~/Desktop`, is read directly from the host only when the developer supplies its path; the agent cannot request one. Administrators control attachments in **Settings → Prompts**; see [Policies](#policies).

The tools the backend can ask the client for are limited to reading files, editing files, and running commands inside the sandbox. The agent cannot load tools of its own, so it cannot extend its reach beyond what the sandbox and policies allow. MCP servers are not supported yet. They would fit this model as long as the agent cannot choose them: administrators would approve which servers are allowed in the console, and the agent could never add one.

John does not show approval dialogs for normal file and command operations. Frequent prompts cause approval fatigue and provide a false sense of security. Instead, centrally managed policies define what the sandbox can access and change.

Only the current directory is mounted, limiting the files available to commands. It is mounted copy-on-write, so the agent can edit freely without any of it reaching the developer's working directory until the developer applies it. Developers should still use John inside a version-controlled project so applied changes can be reviewed and reverted.

Session commands include `/apply`, `/reset`, `/policies`, `/model`, `/resources`, `/env`, `/resume`, and `/new`. `/env` shows the sandbox's environment definition; `/env init` creates one. Running `john` in a directory carries on that directory's conversation, and says so; `/new` starts a new one, and `/resume` lists earlier conversations in the current directory and resumes the selected session using the client's key. `/model` lists the models your policy groups allow and moves the session to the one you pick, or name, as in `/model haiku`; the backend checks the choice, so it cannot reach a model you were not assigned or one a routing policy narrowed the session away from.

These subcommands are also available outside a session:

- `john login <url>` authenticates against that backend and records it as this machine's setup; `john login standalone` sets the machine up to run john on its own instead.
- `john apply` opens the review-and-apply view for the current directory's sandbox.
- `john console` opens the web console.

Standalone mode runs the backend locally in the client's process and uses the same protocol, policies, and sandbox. Run `john login standalone` once, then `john` from a project directory. Models are added in the console, with your own API key: when there is none yet, `john` and `john login standalone` open the console on the Add model dialog and carry on once a model is added. John does not read provider keys from the environment. `john console` opens the console of a running session, or, with none running, serves it on its own until Ctrl-C. One standalone backend runs per machine at a time.

Before a session starts, a client whose version differs from the backend is asked whether to download the matching build and restart. Declining ends the run. If the backend has no client build for that platform, it allows the session without an update.

Users can open the web console directly or with `john console` to:

- Review their usage and spend.
- Inspect the policies applied to them, and the teams those policies come from.
- Submit and track network access requests.
- Download the client.

## Sandbox

Each sandbox is a full Linux virtual machine. John maintains a separate sandbox for each working directory. The sandbox persists between CLI sessions, preserving installed tools, dependencies, build caches, and other VM state until the user resets it from the CLI.

The working directory uses the same path on the host and in the sandbox: `/Users/bob/project` stays `/Users/bob/project`. This preserves paths in debug information, build caches, and stack traces. Windows paths are mapped to Linux paths, such as `/c/Users/bob/project`.

The sandbox's disks live in `~/.john/sandboxes`, outside the working directory. A disk stored inside the tree it backs would appear inside the sandbox it is running.

The sandbox cannot access the network directly. Its only outbound network path is the John network proxy, which only supports HTTP and HTTPS requests. Other outbound protocols and direct network connections are blocked.

### Resources

Each sandbox has default limits on CPU, memory, disk, and process count. Limits are configured locally for each workspace through the CLI. They are not centrally managed policies or console settings.

```
/resources                          show the current limits
/resources cpu=8                    give the sandbox more cores
/resources memory=16 disk=200       and more memory and disk, in GB
/resources processes=2048           raise the process ceiling
```

Changes take effect when the sandbox next starts. Resetting the sandbox restores the defaults.

### Working Directory

The working directory is mounted copy-on-write. The sandbox sees the real tree and reads it normally, but every write the agent makes — edited files, new files, deletions, build output — lands in an overlay layer inside the sandbox. The host working directory is not modified.

Host edits, additions, and deletions are visible to the next command in the sandbox, unless the sandbox has already written its own copy of a file. That copy takes precedence, preserving unapplied work. The host side of the mount exposes no write operations.

`john apply` (or `/apply` in a session) opens the accumulated diff and writes the developer's selected changes to the host, either in full or by path. Review is part of the apply flow. This lets the agent work without approval prompts for each file write while the developer controls which changes reach the working directory.

Anything not applied stays in the sandbox. It survives between sessions like the rest of the sandbox state, and it is discarded when the developer resets the sandbox.

### Protected Paths

Workspace-protection policies restrict access to matching paths:

- **Excluded** paths, such as `.env`, are not served to the sandbox. Sandbox changes at those paths cannot be applied to the host.
- **Read-only** paths can be read. Changes and deletions are blocked by the mount and checked again at apply.
- **No-delete** paths can be edited. Deletions are blocked by the mount and checked again at apply.

Exclusions are enforced on the host side of the mount, outside the sandbox. New exclusions also take effect in running sessions.

Read-only paths are also presented without write permission. The agent runs as root in the VM and can bypass mount restrictions by writing directly to the overlay's disk, so the apply step checks policies again against paths found by walking the sandbox and working directory. Blocked changes are shown in the diff but cannot be selected or applied, even when explicitly named.

Path matching is case-insensitive on every platform. This prevents alternate casing, such as `.SECURE` for `.secure`, from bypassing protection when changes reach a host with a case-insensitive filesystem. On case-sensitive filesystems, both spellings are protected.

`john apply` runs without a backend, so it enforces the protections the last session stored alongside the sandbox. The paths an administrator protected stay protected when nobody is logged in.

### Sandbox Image

The sandbox image is built from `.john/Dockerfile` in the project, so the tools a project needs — compilers, linters, test runners, internal packages — are installed before the agent starts. The first time john starts at the top of a git repository that has none, it writes one: `debian:stable-slim` with basic developer tools (git, curl, build-essential, ripgrep and a few more), to which the project adds its own toolchain. Anywhere else, a project without one is built from that same starter without it being written down, so every sandbox has the basic tools; `/env init` writes it to the file, to add the project's own.

Edit `.john/Dockerfile` while john runs and john says so, checking the file as it goes; `/rebuild` then rebuilds the sandbox from it and keeps everything not yet applied. A change john was not running for — an edit, a `git pull` — is rebuilt on the next start. When the agent proposes a change to `.john/` and you apply it, john offers to rebuild right away.

The build context is `.john/`. Builds read the host copy of the definition, so changes proposed by the agent take effect only after the developer applies them. See [Sandbox environment](sandbox/readme.md#sandbox-environment) for the design and supported Dockerfile instructions.

The client pulls the base image. For private registries, it uses `~/.docker/config.json` and its credential helpers, or credentials saved with `john registry login <registry>`. Registry credentials stay on the host; only image layers enter the sandbox.

Image builds have unrestricted network access by default so they can install project tooling before a session starts. Administrators can change this organization-wide in **Settings → Sandboxes**: **Proxied** routes build requests through the proxy and applies network policies to them, like any other request from the sandbox.

The setting is included in the signed policy snapshot and displayed by `/policies`. Once the image contains the required tools, sessions can use them with outbound network access blocked.

## Network Proxy

The network proxy runs locally on the developer's computer. It buffers an entire outbound HTTP or HTTPS request before sending any part of it to its destination, then checks the complete request against the policies provided by the backend for the user's teams and the policy groups attached to them.

The proxy runs outside the sandbox VM and is configured as the sandbox machine-wide HTTP and HTTPS proxy. When a sandbox is created, the client generates a certificate authority for it on the developer's machine and installs it in that VM's trust store, which allows the proxy to terminate and inspect TLS connections and enforce backend-provided policies at the HTTP request level. The CA is created on the developer's machine and stays there — the backend neither holds it nor distributes it.

Every request is evaluated independently, including requests caused by redirects. Policies can separately match request methods, URL components, selected headers, and query parameters. Request bodies are buffered so ask-developer requests can be inspected, but policy does not content-match them; policies can instead set a maximum body size. Protocol upgrades, compressed bodies, multipart requests, and other request forms that could bypass inspection are blocked unless explicitly supported.

Every request gets one of four outcomes. Three of them come from a policy that matched it:

- **Always allow:** The proxy forwards the request without interrupting the developer.
- **Ask developer:** The proxy holds the buffered request and shows it to the developer, who allows or rejects **that one request**. The answer is about the request in front of them and nothing else.
- **Deny:** The proxy refuses the request outright, without asking anyone. An administrator has already decided.

The fourth is the absence of a policy. A request nothing matches is **unknown**, and unknown means blocked the moment it is made:

- **Unknown:** The proxy refuses the request, and John then asks the developer whether to file a network access request so a policy can cover requests like it. Filing one never unblocks the request that was refused — that one has already failed, and a policy is an administrator's decision about what happens next time. The developer can also start the same request from the console.

Unknown is the only outcome a developer can act on. An allowed request needs nothing, and a denied one has already been considered.

## Policies

Policies are deterministic controls enforced by the John client and backend. They are not instructions that the model is expected to follow.

Policies are organized into policy groups, and policy groups are attached to teams. A policy group bundles related policies — such as secret redaction, a read-only network baseline, or an internal-models-only routing rule — so a control is defined once and applied wherever it is needed. Users can request network access for their teams, and administrators approve or reject those requests (deciding if they go into new or existing groups). Every policy and group change is versioned and recorded.

A policy group can be marked **baseline**. Baseline groups are attached to all teams automatically and cannot be detached or weakened by any other group, establishing the floor that every team works inside. All other groups are attached to teams individually and configure access within that floor. When restrictive policies conflict, the most restrictive result wins; additive assignments combine.

Policies match the concrete details of an operation or request. For network requests, policies can match attributes such as the HTTP method, full URL, scheme, host, port, path, selected headers, and query parameters, rather than only allowing or denying a broad destination. This lets policies permit narrowly defined operations, such as `GET` requests to a specific API path, without permitting unrelated requests to the same host.

Model-routing policies assign approved models through baseline or team-attached groups. A user can use the union of models assigned by the groups attached to their teams, less anything a baseline restriction removes.

A routing policy can narrow a running session based on keywords. Before each model request, the backend checks all outgoing text, including prompts, file contents, command output, and diffs. A match restricts the session to the policy's permitted models and asks the developer which remaining model to use. Restrictions last for the rest of the session. Keyword matching is deterministic and can be inspected and tested.

Supported policy types include:

- **Replacements:** The backend replaces content matching a regular expression in every outgoing model request. A `#` in the replacement becomes a stable alias: repeated values receive the same placeholder, and distinct values receive different placeholders.
- **Model routing:** Assign approved models through baseline or team-attached policy groups, and narrow a session to a subset when the backend sees a listed keyword in what it is about to send to a model. For example, a team can be assigned several models while any session containing C++ continues on an internally hosted model only.
- **Workspace protection:** Exclude matching files and directories from the sandbox, mount them as read-only, or prevent their deletion. Common protected paths include `.git`, `.env`, and `.github/workflows`.
- **Prompt attachments:** An organization-wide setting in **Settings → Prompts**, included in the signed snapshot, controls whether prompts can carry images or PDFs. Replacements and routing keywords cannot inspect these files, so the default, **guarded**, blocks attachments in sessions subject to either control. Administrators can choose **always** or **never** instead. Traces record each attachment's name, type, size, and SHA-256, but not its bytes.
- **Network access:** Restrict outbound HTTP and HTTPS requests by method, URL, scheme, host, port, path, selected headers, query parameters, and maximum body size. Request bodies are not content-matched. Matching policies decide between always allow, ask developer, and deny; unmatched requests are unknown, which means blocked, and are the one thing a developer can submit a network access request for.

## Security

- **Sandboxed execution:** Commands run in a full, isolated Linux virtual machine, with only permitted workspace paths mounted.
- **Copy-on-write workspace:** The working directory is mounted copy-on-write. The agent's edits accumulate in the sandbox and reach the developer's working directory only after the developer reviews the diff and applies it.
- **No host credentials:** Host environment variables, SSH agents, Docker sockets, devices, and unrelated files are not available inside the sandbox.
- **Proxy-only network access:** The sandbox has no direct network access. Outbound HTTP and HTTPS requests must pass through the network proxy, and other protocols are blocked.
- **Controlled exceptions:** Unknown requests are blocked the moment they are made. A developer can ask for a policy, but an administrator decides it, and it only ever affects later attempts. Ask-developer requests require the user to inspect and approve that individual request.
- **The loop is not on the laptop:** The agent loop runs in the backend. The client executes tool calls and holds no model credentials, so there is no second path to a provider.
- **Deterministic enforcement:** Authorization and policy decisions are enforced outside the model.
- **Data controls:** Policies can rewrite sensitive content, and route it to an internally hosted model, before it reaches a remote one.
- **Protected workspaces:** Policies can prevent access to or modification of sensitive project files.
- **Credentials in the operating system's keychain:** The machine token `john login` receives and any registry password `john registry login` was given go to the macOS Keychain, Windows Credential Manager, or the desktop keyring on Linux. A machine with none of those keeps them in a 0600 file. What is not secret — which backend this machine is signed in to, as whom, which registries it knows — stays readable in `~/.john` either way.
- **Encrypted session traces:** The backend stores a complete trace of every session, encrypted with a per-session key that is wrapped for the organization and for the developer the session belongs to. Trace writers can encrypt records but cannot decrypt them: administrators holding the private read key can, and so can developers, for their own sessions.

### Threat Model

John is designed to reduce the risks created by an agent having access to private data, being exposed to untrusted instructions, and having a means to communicate data externally (lethal trifecta). John constrains each of these capabilities, but does not assume that any of them can be eliminated completely.

An agent may encounter malicious instructions in repository files, dependencies, command output, compiler or linter output, API responses, fetched web content, or generated files. These inputs are treated as untrusted data, not as policy or authority.

John restricts the data available to the agent and LLMs by isolating the workspace in a sandbox, excluding host credentials, and applying workspace and model-routing policies.

It limits exfiltration through proxy-only network access and by keeping the agent loop and every model call in the backend.

It limits blast radius through sandbox isolation, the copy-on-write working directory, and deterministic policies enforced outside the model. Because the agent's writes stay in the sandbox until they are reviewed, a successful injection that edits code, build scripts, or CI configuration produces a diff for the developer to reject rather than a change already on disk.

John assumes prompt injection may influence the model. Sensitive operations therefore remain restricted by deterministic controls enforced outside the model.

### Known Risks

- **Sensitive data reaching an LLM provider (High):** Any project content included in a remote model request may leave the organization. Replacements reduce this risk but cannot guarantee that every sensitive value is detected. Restricted projects should use internally hosted models.
- **Prompt injection from untrusted content (High):** Repository files, documentation, dependencies, command output, compiler or linter output, API responses, fetched web content, and generated files may contain instructions intended to manipulate the agent. Sandboxing and deterministic policies limit the impact but do not prevent prompt injection.
- **Changes applied without real review (Medium):** The copy-on-write working directory means nothing reaches the developer's files until they apply it, so the protection is only as good as the review. A large diff approved quickly is the same failure that per-write approval prompts produce, just relocated. Keeping tasks small enough that their diffs are readable is the mitigation.
- **Persistent malicious changes (Medium):** The agent may introduce unsafe code, build scripts, dependency changes, or CI configuration. These now surface as diff lines at the apply step rather than as files already written, but a change that is applied and committed without being read is still a change that shipped. Protecting sensitive paths reduces the risk further.
- **Allowed request abuse (Medium):** An endpoint permitted by a network policy may serve malicious content or accept uploaded data. An allowed host can become an exfiltration path through request bodies, query parameters, headers, redirects, or upload-capable methods. Policies should default to read-only methods, separately approve write and upload operations, constrain paths and request sizes, and continuously review allowed destinations.
- **Content no text control can read (Medium):** Replacements and routing keywords work by reading what is on its way to a model, so an image or a PDF passes them untouched — a screenshot of a screen whose text would have been redacted is not redacted. The default attachment setting refuses attachments in exactly the sessions those controls apply to, and an organization that allows them anyway is making that trade knowingly. Attachments are recorded in the trace by name, type, size and digest, so what was sent can be established afterwards.
- **Unapplied work lost on reset (Low):** Changes the developer has not applied live only in the sandbox overlay. Resetting the sandbox discards them.

## Backend

The self-hosted backend is a service that uses SQLite and acts as the control plane for every John client.

It runs the agent loop and manages authentication, authorization, teams, policies, approvals, network access, model routing, usage, session traces, and the history of its own configuration. Developers can inspect their own access and usage. Through the admin interface, admins can decide requests, manage policies and policy groups, configure models, attach groups to teams, read the change history, and read the stored traces.

The result is one governed interface between developers, coding agents, internal systems, and external model providers.

### Session Traces

The backend keeps a trace of every session. Each session is encrypted with its own key, and copies of that key — wrapped for the organization, and for the developer the session belongs to — are written into the trace file's own header. The backend holds public keys only: it mints a session key, uses it while the session is live, and once the key leaves memory it can never read the file back. A restored trace store needs a private key and nothing else, not a matching database.

The claim, stated as narrowly as it is true: **the backend can read a session while a client is connected to it, and cannot read it before or after.** It builds every model request, so it must see the plaintext to work; what it gives up is the ability to read that plaintext at rest. A stolen database, a stolen backup, or a server compromised tomorrow yields nothing about yesterday.

Two people can read a session. **Administrators** unlock traces in the admin interface with the organization's private read key, which is held outside the backend and never sent to it; records are decrypted in the browser as they are read. **Developers** read their own sessions with the key their client generated at sign-in and keeps on their machine — that is what `/resume` is built on. Losing that key costs read access and nothing else: an administrator can re-wrap a session's key for a new one.

The session's stored conversation is sealed with the same key, so resuming it means the client hands the key back — never the conversation, which stays the backend's to author. Traces are kept forever. There is no retention setting and nothing expires them on its own — an operator who wants them gone deletes them from the trace store.

The key hierarchy, the file format, what the encryption does and does not protect against, and its known limits are in [trace/readme.md](trace/readme.md).

### Change History

Separately from the traces, the backend keeps a record of every change to its own configuration: a policy written or disabled, a group created, a group attached to a team, membership changed, a model added, a request approved or rejected, a setting altered. Each entry says who made the change and when, and the version it produced. Nothing amends or deletes an entry.

This record is **not** encrypted, and that is the point. Session traces hold what developers wrote and what models answered, so they are sealed behind a read key held outside the backend. The change history holds no session content at all — only what happened to the controls — so any administrator can answer "who loosened this, and when" without unlocking anything, and without being trusted to read everyone's work.

## Administration

There are two roles: **member** and **administrator**. Members write code, and can request network access for their teams. Administrators own policy and operate John through the admin interface in the web console. A typical first-run sequence:

1. **Sign in** as the mapped initial administrator and confirm single sign-on is working.
2. **Create teams** and assign members.
3. **Define baseline policy groups.** These establish the constraints every team must respect.
4. **Build and attach policy groups** to teams — network access, workspace protection and replacements.
5. **Assign models** through baseline or team-attached groups, with any keyword-triggered routing rules.
6. **Handle requests.** Review and approve or reject the network access requests submitted by developers, and read session traces and the change history as needed.

Baseline groups cannot be weakened by other groups; where policies conflict, the most restrictive result wins. See [Policies](#policies) for the full model.

## Technical Details

- **One executable:** The client and backend are a single Go binary selected by subcommand — `john`, `john login`, `john backend`. The web console is embedded in it. Linux and Windows builds use `CGO_ENABLED=0`. macOS clients require cgo, the Apple SDK, and signing with the virtualization entitlement, so the full release matrix is built on a Mac. See the [Makefile](Makefile) for build targets.
- **CLI:** Runs on macOS, Windows and Linux.
- **Network proxy:** Runs locally on the developer's computer and enforces policies provided by the backend.
- **Backend:** Runs on Linux, macOS or Windows — a Linux server is the usual home — and uses SQLite. Runs the agent loop.
- **Models:** Configured by administrators through the admin interface and called only by the backend.
- **Client/backend protocol:** One WebSocket per session; see [`protocol/readme.md`](protocol/readme.md).

John fails closed. Policy decisions require the backend, so when the client or proxy cannot reach it, operations that depend on a policy decision are blocked rather than allowed.

The client caches network policies for a few minutes so brief backend outages do not interrupt an in-flight command. Once the cache expires, requests that need a policy decision are blocked until the backend is reachable again. Prompts stop immediately when the backend is unreachable, because the loop that answers them lives there.

## Requirements

**Developer workstation:**

- macOS (Apple Silicon or Intel), Windows (x86-64), or Linux (x86-64, or arm64 as experimental: it is built but has not yet been run on real arm64 hardware), with hardware virtualization enabled. Each sandbox is a virtual machine running directly on the platform hypervisor — Virtualization.framework on macOS, Hyper-V on Windows, KVM on Linux.
- On Linux, `/dev/kvm` readable by the developer (usually membership of the `kvm` group) and [libkrun](https://github.com/containers/libkrun) 1.17 or later, built with its block device (`BLK=1`). It is the one part of the sandbox that is not inside the client: KVM gives out processors and memory and leaves the virtual machine to be built, and libkrun is what builds it. John says which of the two is missing when it cannot start a sandbox. Fedora packages libkrun (`dnf install libkrun`). Debian and Ubuntu do not, so build a release — it needs a newer Rust than they ship, with rustfmt and the musl target, and installs to a directory their loader does not search unless told otherwise:

  ```sh
  sudo apt install build-essential git curl libclang-dev
  curl -fsSL https://sh.rustup.rs | sh -s -- -y
  . ~/.cargo/env && rustup component add rustfmt && rustup target add $(uname -m)-unknown-linux-musl
  git clone -b v1.19.5 https://github.com/containers/libkrun && cd libkrun
  make BLK=1 && sudo make BLK=1 LIBDIR_Linux=lib install && sudo ldconfig
  sudo usermod -aG kvm $USER   # then sign in again
  ```
- Disk capacity for one persistent sandbox per working directory, plus the base image. The base image ships inside the client, so nothing else has to be fetched to start.
- Network access to the organization's John backend.

**Backend host:**

- A server reachable by every client. Linux is the usual choice; the macOS and Windows builds run the backend too.
- Persistent storage for the SQLite database and the encrypted session traces.

## License

John is released under the [MIT License](LICENSE). To report a vulnerability, see [SECURITY.md](SECURITY.md).
