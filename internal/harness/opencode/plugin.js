// murmur's plugin for OpenCode. murmur join writes it and murmur uninstall removes it; edits here don't last.
// It tells murmur when a session starts, works, waits for its person and ends, asks murmur before each tool call
// (an edit of a file another agent changes is turned back once), gives the agent murmur's who, send and btw tools,
// and hands a session the messages murmur sends to the socket it listens on.
import { spawn } from "node:child_process"
import { createServer } from "node:net"
import { chmodSync, rmSync } from "node:fs"
import { randomBytes } from "node:crypto"

const MURMUR = "__MURMUR__"
const SOCKETS = "__SOCKETS__"

// run runs murmur with these arguments and input as JSON on its stdin, and resolves to what it prints. It never
// fails: a murmur that doesn't answer must not hold a session up. Once signal aborts (its person stopped the agent),
// murmur is stopped and it resolves at once: a btw that stops waiting so has its answer come as a message instead.
function run(args, input, signal) {
  return new Promise((resolve) => {
    let out = ""
    let child
    if (signal?.aborted) return resolve("")
    try {
      child = spawn(MURMUR, args, { stdio: ["pipe", "pipe", "ignore"] })
    } catch {
      resolve("")
      return
    }
    const stop = () => {
      child.kill()
      resolve("")
    }
    signal?.addEventListener("abort", stop, { once: true })
    child.stdout.on("data", (d) => (out += d))
    child.on("error", () => resolve(""))
    child.on("close", () => {
      signal?.removeEventListener("abort", stop)
      resolve(out.trim())
    })
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
  const tool = (name, id, more = {}, signal) =>
    run(["tool", "opencode", name], { session_id: top(id), pid: process.pid, ...more }, signal)

  // murmur ends its side once the message is written, and waits for the answer
  const server = createServer({ allowHalfOpen: true }, (conn) => {
    const chunks = []
    let size = 0
    conn.on("data", (d) => {
      size += d.length
      if (size > 1 << 16) return conn.destroy() // a message is a few kilobytes
      chunks.push(d)
    })
    conn.on("error", () => {})
    conn.on("end", async () => {
      try {
        const m = JSON.parse(Buffer.concat(chunks).toString("utf8")) // whole, so no character is cut in two
        const res = await client.session.promptAsync({
          path: { id: m.session },
          body: { parts: [{ type: "text", text: m.text }], agent: agent.get(m.session) },
          throwOnError: true,
        })
        if (res?.error) throw new Error(res.error?.data?.message ?? res.error?.message ?? JSON.stringify(res.error))
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
          "The agent reads it between two of its tool calls. Make the first line say what the message is about. Use it when another agent's work affects yours, to answer a [murmur] message, or when a decision needs a specific person. " +
          "When your part is done, say so on your last message with last rather than in a message of its own: a thank-you interrupts the other agent for nothing.",
        args: {
          to: { type: "string", description: "person/agent for an agent, or a person's handle" },
          text: { type: "string", description: "the message; plain text" },
          topic: {
            type: "string",
            description:
              'what the exchange is about, in a few words ("Release freeze tonight"), the same on every message of it: answering a [murmur] message, reuse its topic unless the subject moved',
          },
          last: {
            type: "boolean",
            description:
              "true when this message ends your part of the exchange and you expect no answer: an answer that settles the question, an agreement, a hand-off. Murmur stops showing you together; a reply brings you back",
          },
        },
        execute: async (args, ctx) =>
          tool("send", ctx.sessionID, { to: args.to, text: args.text, topic: args.topic, last: args.last }),
      },
      murmur_btw: {
        description:
          'Ask a coding agent a quick question by the way (to: "bob/main-1"), without interrupting it: a copy of its conversation answers, ' +
          "and the agent itself never sees the question. Use it to learn what another agent is doing, has found or knows. The answer comes back " +
          "within a minute, or later as a [murmur] message when it takes longer (a teammate may approve answers first). Use murmur_send " +
          "instead when the other agent must act on something or answer itself.",
        args: {
          to: { type: "string", description: "person/agent: the agent to ask" },
          question: { type: "string", description: "the question; plain text" },
        },
        execute: async (args, ctx) => tool("btw", ctx.sessionID, { to: args.to, question: args.question }, ctx.abort),
      },
    },
  }
}
