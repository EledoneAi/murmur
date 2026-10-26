# What leaves a laptop, and who sees it

With `murmur solo`, nothing does: the hub is the laptop's own daemon, on 127.0.0.1.

In a team, everything goes to your team's hub, and only there: murmur has no central service. If your admins set a chat webhook, the hub posts to your team's chat the agents left waiting (the person's handle, the agent's name, repo and branch) and a weekly report in team totals, which names the files changed in two checkouts at once most often.

- **Everyone on the team**: your handle and name; for each agent its name, repo, branch, pull request and its
  address, the title its coding tool or its person gave its conversation (or, once its focus moved on, the one a copy
  of the conversation gave it on your laptop, unless you run `murmur retitles off`), its status (busy, idle, waiting, or in a
  shell), whether it waits for a permission or an answer and since when, where it runs (`vscode`, `desktop`, `sdk`,
  or the coding tool, such as `codex`), and what it does now as a tool and a file, a command name, a skill, or an
  MCP server and tool ("Editing routes.go", "Running go", "Using pdf", "github · create_issue"). Also the kind of each of its last 30 tool calls (read, edit, run, web,
  subagent, message) and whether the same command keeps coming back, how full its context window is, how many lines
  and files its branch changes, the local branch it is stacked on, a detached commit or a rebase in progress, the
  folder it runs in under its checkout, the names of other repos it touched, and whether teammates can ask it
  something by the way and whether its answers wait for you. Announcements you send to a repo stay on the team page
  for a day.
- **The hub only, for overlaps**: the paths of files your checkouts change (commits no remote has, changed and
  untracked files), and the path of each file an agent is about to edit, as it checks it. When two checkouts
  change the same file, their people see it in the console, and an agent about to edit it hears which other agent
  changes it.
- **The people in a conversation**: its messages. A message kept for someone offline waits at most 7 days.
- **The person who asks an agent something by the way, or whose agent asks, and the agent's person**: the question,
  where it stands ("answered; waits for bob to share it"), the answer, and how much the copy that answered read, in
  tokens. The agent's person sees who asked: a teammate, or a teammate's agent ("carol's agent web-2"). An agent's
  question shows only on the laptop that asks and the laptop that answers, and links the two agents on the board only
  there. The agent asked never sees the question. A throwaway copy of the agent's conversation writes the answer, and
  it can repeat anything the conversation holds, so it leaves your laptop only as you allow: an answer to a teammate
  or a teammate's agent waits for you to share it or drop it, unless you chose `murmur btw-answers auto` or `all`, and the
  console shares it only once all of it has been on your screen (the plain mode prints it whole); with `off`, they
  can't ask: your laptop and the hub both refuse them. Answers to your own questions and your agents', from any of
  your laptops, don't wait, and a question from you or your agent to your agent on the same laptop doesn't leave it.
  The hub passes the question and the answer between two laptops and keeps neither in its store, nor for a laptop
  that is offline; for a day, in memory, it remembers which laptops a question went between, never what it said; its
  logs say who asked which agent, never what was asked or answered. The copy runs on your laptop, with the agent's
  own Claude Code and your Claude account or cloud provider, which bills it as it bills the agent. It runs no tools,
  so it reads no file and runs no command, and a file the question names with `@` isn't attached to it; the agent's
  conversation stays as it was. A hook turns back each tool call and counts it: an answer from a copy that used a
  tool the hook never saw is dropped. Murmur copies only sessions of a Claude Code it checked copies with, 2.1.292 or
  later. Where your organisation's managed Claude Code settings turn hooks off (`disableAllHooks`,
  `allowManagedHooksOnly`), no copy runs, and teammates can't ask your agents. To start it, the daemon reads the
  agent's sign-in settings (its cloud provider, region and keys) from the agent's process and your Claude Code
  settings, and never keeps, logs or sends them. It hands them to the copy in its environment and in a file only you
  can read, in murmur's folder, removed once the copy is done, or as murmur starts again when it stopped first (it
  stops its copies as it stops); never on its command line, which other users of the laptop can see. A process that
  only has the agent's process number now, the agent having ended, is never read. When the copy fails, the asker
  hears only that it failed on your laptop; you see why.
- **The agent that asks**: the answer, which enters its context and stays in its conversation. It comes as a message
  does, marked as relayed from another session, not from the agent's person, so it can't approve anything, and it
  says a copy of the other agent's conversation wrote it. One that comes after the agent's `btw` stopped waiting, or
  after its call was cancelled, reaches it as a message from the agent asked, which says a copy wrote it and the
  agent asked never saw the question; if the agent closes before it reads it, nobody hears of it, the agent asked
  least of all. Why there is no answer, and where the question stands, come quoted, as relayed by murmur: the other
  laptop or the hub wrote them, not murmur. An agent's question holds what the agent chose to write, as its messages
  do.
- **You and your team's admins**: each laptop's name, sent at sign-in; its murmur version and when it was
  last seen; and its self-check, once a day by default: OS and CPU, the versions of its coding tools, and the setup
  checks `murmur doctor` runs.
- **You only, on your laptop**: the first line of what each of your agents last said in its own words, which your
  console shows you. It never leaves the laptop.
- **Never**: prompts, transcripts, code, diffs, command lines, search patterns, the web pages agents search or
  read, to-do lists or costs. The one way any of these leaves is in an answer by the way, written by a copy of a
  conversation, as its person allows.

A hub with more than one instance passes messages, questions and answers between its instances on its bus. The
Postgres bus hands an event of 8000 bytes or more, such as a long message, question or answer, to the other instances
through a table, `murmur_bus`, which deletes it once it is five minutes old. Redis and a single instance keep nothing,
and a single instance publishes no answer by the way.
