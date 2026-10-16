// murmur's plugin for OpenCode. murmur join writes it and murmur uninstall removes it; edits here don't last.
// It tells murmur when a session starts, works, waits for its person and ends, asks murmur before each tool call
// (an edit of a file another agent changes is turned back once), gives the agent murmur's who and send tools, and
// hands a session the messages murmur sends to the socket it listens on.
import { spawn } from "node:child_process"
import { createServer } from "node:net"
import { chmodSync, rmSync } from "node:fs"
import { randomBytes } from "node:crypto"

const MURMUR = "__MURMUR__"
const SOCKETS = "__SOCKETS__"

// run runs murmur with these arguments and input as JSON on its stdin, and resolves to what it prints. It never
// fails: a murmur that doesn't answer must not hold a session up.
function run(args, input) {
  return new Promise((resolve) => {
    let out = ""
    let child
    try {
      child = spawn(MURMUR, args, { stdio: ["pipe", "pipe", "ignore"] })
    } catch {
      resolve("")
      return
    }
    child.stdout.on("data", (d) => (out += d))
    child.on("error", () => resolve(""))
    child.on("close", () => resolve(out.trim()))
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(input))
  })
}

export const Murmur = async ({ client, directory }) => {
  const parent = new Map() // a subagent's session -> the session it works for
  const agent = new Map() // a session -> its agent (build, plan…), which murmur's messages keep
  const brief = new Map() // a session -> what murmur tells it as it starts, for its system prompt, once known
  const title = new Map()
  const top = (id) => parent.get(id) ?? id
  // one OpenCode process may load the plugin once per folder it serves: each copy listens on a socket of its own
  const sock = `${SOCKETS}/opencode-${process.pid}-${randomBytes(4).toString("hex")}.sock`
  const hook = (event, id, more = {}) =>
    run(["hook", "opencode", event], { session_id: id, cwd: directory, pid: process.pid, socket: sock, title: title.get(id), ...more })
  const tool = (name, id, more = {}) => run(["tool", "opencode", name], { session_id: top(id), pid: process.pid, ...more })

  // murmur ends its side once the message is written, and waits for the answer
  const server = createServer({ allowHalfOpen: true }, (conn) => {
    let data = ""
    conn.on("data", (d) => {
      data += d
      if (data.length > 1 << 16) conn.destroy() // a message is a few kilobytes
    })
    conn.on("error", () => {})
    conn.on("end", async () => {
      try {
        const m = JSON.parse(data)
        await client.session.promptAsync({
          path: { id: m.session },
          body: { parts: [{ type: "text", text: m.text }], agent: agent.get(m.session) },
        })
        conn.end("ok\n")
      } catch (e) {
        conn.end(`error: ${e?.message ?? e}\n`) // murmur tells the sender
      }
    })
  })
  server.on("error", () => {})
  server.listen(sock, () => chmodSync(sock, 0o600))
  server.unref?.()
  process.on("exit", () => rmSync(sock, { force: true }))
  // the brief, fetched once per session: when the session starts, or before its first request if that comes first
  const briefOf = (id) => {
    if (!brief.has(id)) brief.set(id, hook("start", id))
    return brief.get(id)
  }

  return {
    event: async ({ event }) => {
      const p = event.properties ?? {}
      const id = p.sessionID ?? p.info?.id
      if (!id) return
      if (event.type === "session.created" && p.info?.parentID) {
        parent.set(id, top(p.info.parentID))
        return
      }
      if (parent.has(id)) return // a subagent's session: the one it works for says it all
      switch (event.type) {
        case "session.created":
          await briefOf(id)
          break
        case "session.updated":
          if (p.info?.title) title.set(id, p.info.title)
          break
        case "session.status":
          if (p.status?.type === "busy") await hook("prompt", id)
          break
        case "session.idle":
          await hook("stop", id)
          break
        case "permission.asked":
          await hook("needs", id, { needs: "permission" })
          break
        case "session.deleted":
          await hook("end", id)
          break
      }
    },
    "chat.message": async (input) => {
      if (input.agent) agent.set(input.sessionID, input.agent)
    },
    "experimental.chat.system.transform": async (input, output) => {
      const b = input.sessionID && (await briefOf(top(input.sessionID)))
      if (b) output.system.push(b)
    },
    "tool.execute.before": async (input, output) => {
      const why = await hook("before-tool", top(input.sessionID), { tool: input.tool, args: output.args })
      if (why) throw new Error(why) // the agent reads why its edit was turned back
    },
    tool: {
      murmur_who: {
        description:
          "List your teammates and their coding agents on murmur: what each agent works on, its repo and branch, its pull request, and whether it is working, idle or waiting for its person.",
        args: {},
        execute: async (_args, ctx) => tool("who", ctx.sessionID),
      },
      murmur_send: {
        description:
          'Send a message to a teammate\'s coding agent (to: "bob/main-1") or to a teammate (to: "bob"), who sees it in their murmur console. ' +
          "The agent reads it between two of its tool calls. Make the first line say what the message is about. Use it when another agent's work affects yours, to answer a [murmur] message, or when a decision needs a specific person.",
        args: {
          to: { type: "string", description: "person/agent for an agent, or a person's handle" },
          text: { type: "string", description: "the message; plain text" },
        },
        execute: async (args, ctx) => tool("send", ctx.sessionID, { to: args.to, text: args.text }),
      },
    },
  }
}
