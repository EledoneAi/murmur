# What leaves a laptop, and who sees it

Everything goes to your team's hub, and only there: murmur has no central service. If your admins set a chat
webhook, the hub posts to your team's chat the agents left waiting (the person's handle, the agent's name, repo and
branch) and a weekly report in team totals, which names the files changed in two checkouts at once most often.

- **Everyone on the team**: your handle and name; for each agent its name, repo, branch, pull request and its
  address, the title Claude Code gave its conversation, its status (busy, idle, waiting, or in a shell), whether it
  waits for a permission or an answer and since when, where it runs (`vscode`, `desktop`, `sdk`), and what it does
  now as a tool and a file, a command name, a skill, or an MCP server and tool ("Editing routes.go", "Running go",
  "Using pdf", "github · create_issue"). Also the kind of each of its last 30 tool calls (read, edit, run, web,
  subagent, message) and whether the same command keeps coming back, how full its context window is, how many lines
  and files its branch changes, the local branch it is stacked on, a detached commit or a rebase in progress, the
  folder it runs in under its checkout, and the names of other repos it touched. Announcements you send to a repo
  stay on the team page for a day.
- **The hub only, for overlaps**: the paths of files your checkouts change (commits no remote has, changed and
  untracked files), and the path of each file an agent is about to edit, as it checks it. When two checkouts
  change the same file, their people see it in the console, and an agent about to edit it hears which other agent
  changes it.
- **The people in a conversation**: its messages. A message kept for someone offline waits at most 7 days.
- **You and your team's admins**: each laptop's name, sent at sign-in; its murmur version and when it was
  last seen; and its self-check, once a day by default: OS and CPU, Claude Code's version, and the setup checks
  `murmur doctor` runs.
- **Never**: prompts, transcripts, code, diffs, command lines, search patterns, the web pages agents search or
  read, to-do lists or costs.
