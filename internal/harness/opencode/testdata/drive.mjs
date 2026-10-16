// Drives murmur's OpenCode plugin as OpenCode does, with a client that records the prompts it gets, and prints what
// happened as JSON. Payloads follow OpenCode's plugin API (packages/plugin/src/index.ts, v1.19).
import { connect } from "node:net"
import { readdirSync, statSync } from "node:fs"
import { pathToFileURL } from "node:url"

const [plugin, sockets] = process.argv.slice(2)
const { Murmur } = await import(pathToFileURL(plugin).href)
const prompts = []
const client = { session: { promptAsync: async (req) => prompts.push(req) } }
const hooks = await Murmur({ client, directory: "/work/app", worktree: "/work/app", project: {} })
await new Promise((r) => setTimeout(r, 50)) // the socket listens
const out = {}

// the first request of a session can come before its session.created: its brief is fetched then, and awaited
const first = []
await hooks["experimental.chat.system.transform"]({ sessionID: "s0", model: {} }, { system: first })
out.first = first

await hooks.event({ event: { type: "session.created", properties: { sessionID: "s1", info: { id: "s1", title: "New session" } } } })
await hooks.event({ event: { type: "session.created", properties: { sessionID: "s2", info: { id: "s2", parentID: "s1" } } } })
await hooks.event({ event: { type: "session.status", properties: { sessionID: "s2", status: { type: "busy" } } } })
await hooks["chat.message"]({ sessionID: "s1", agent: "plan" }, { message: {}, parts: [] })

const system = []
await hooks["experimental.chat.system.transform"]({ sessionID: "s2", model: {} }, { system })
out.system = system
try {
  await hooks["tool.execute.before"]({ tool: "edit", sessionID: "s2", callID: "c1" }, { args: { filePath: "/work/app/x.go", oldString: "a", newString: "b" } })
  out.edit = ""
} catch (e) {
  out.edit = e.message
}
out.who = await hooks.tool.murmur_who.execute({}, { sessionID: "s2", messageID: "m", agent: "plan", abort: new AbortController().signal })

const socks = readdirSync(sockets).filter((f) => f.startsWith(`opencode-${process.pid}-`) && f.endsWith(".sock"))
out.sockets = socks.length
out.mode = (statSync(`${sockets}/${socks[0]}`).mode & 0o777).toString(8)
out.reply = await new Promise((resolve) => {
  let reply = ""
  const c = connect(`${sockets}/${socks[0]}`, () => c.end(JSON.stringify({ session: "s1", text: "[murmur] bob: hi" })))
  c.on("data", (d) => (reply += d))
  c.on("close", () => resolve(reply.trim()))
  c.on("error", () => resolve("connect failed"))
})
out.prompts = prompts
console.log(JSON.stringify(out))
process.exit(0)
