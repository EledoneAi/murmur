// Drives murmur's OpenCode plugin as OpenCode does, with a client that records the prompts it gets, and prints what
// happened as JSON. Payloads follow OpenCode's plugin API (packages/plugin/src/index.ts, v1.19).
import { connect } from "node:net"
import { pathToFileURL } from "node:url"

const [plugin, sockets] = process.argv.slice(2)
const { Murmur } = await import(pathToFileURL(plugin).href)
const prompts = []
const client = { session: { promptAsync: async (req) => prompts.push(req) } }
const hooks = await Murmur({ client, directory: "/work/app", worktree: "/work/app", project: {} })
const out = {}

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

await new Promise((resolve) => {
  const c = connect(`${sockets}/opencode-${process.pid}.sock`, () => c.end(JSON.stringify({ session: "s1", text: "[murmur] bob: hi" })))
  c.on("close", resolve)
  c.on("error", resolve)
})
for (let i = 0; i < 50 && prompts.length === 0; i++) await new Promise((r) => setTimeout(r, 20))
out.prompts = prompts
console.log(JSON.stringify(out))
process.exit(0)
